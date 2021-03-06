package main

import (
	"fmt"
	"net"
	"time"

	"sync"

	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

const udpBufSize = 64 * 1024

// Listen on laddr for UDP packets, encrypt and send to server to reach target.
func udpLocal(laddr, server, target string, ciph core.PacketConnCipher) {
	srvAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		logf("UDP server address error: %v", err)
		return
	}

	tgt := socks.ParseAddr(target)
	if tgt == nil {
		err = fmt.Errorf("invalid target address: %q", target)
		logf("UDP target address error: %v", err)
		return
	}

	c, err := net.ListenPacket("udp", laddr)
	if err != nil {
		logf("UDP local listen error: %v", err)
		return
	}
	defer c.Close()

	nm := newNATmap(config.UDPTimeout)
	buf := make([]byte, udpBufSize)
	copy(buf, tgt)

	logf("UDP tunnel %s <-> %s <-> %s", laddr, server, target)
	for {
		n, raddr, err := c.ReadFrom(buf[len(tgt):])
		if err != nil {
			logf("UDP local read error: %v", err)
			continue
		}

		pc := nm.Get(raddr.String())
		if pc == nil {
			pc, err = net.ListenPacket("udp", "")
			if err != nil {
				logf("UDP local listen error: %v", err)
				continue
			}

			pc = ciph.PacketConn(pc)
			nm.Add(raddr, c, pc)
		}

		_, err = pc.WriteTo(buf[:len(tgt)+n], srvAddr)
		if err != nil {
			logf("UDP local write error: %v", err)
			continue
		}
	}
}

// Listen on addr for encrypted packets and basically do UDP NAT.
func udpRemote(addr string, ciph core.PacketConnCipher) {
	c, err := core.ListenPacket("udp", addr, ciph)
	if err != nil {
		logf("UDP remote listen error: %v", err)
		return
	}
	defer c.Close()

	nm := newNATmap(config.UDPTimeout)
	buf := make([]byte, udpBufSize)

	logf("listening UDP on %s", addr)
	for {
		n, raddr, err := c.ReadFrom(buf)
		if err != nil {
			logf("UDP remote read error: %v", err)
			continue
		}

		tgtAddr := socks.SplitAddr(buf[:n])
		if tgtAddr == nil {
			logf("failed to split target address from packet: %q", buf[:n])
			continue
		}

		tgtUDPAddr, err := net.ResolveUDPAddr("udp", tgtAddr.String())
		if err != nil {
			logf("failed to resolve target UDP address: %v", err)
			continue
		}

		payload := buf[len(tgtAddr):n]

		pc := nm.Get(raddr.String())
		if pc == nil {
			pc, err = net.ListenPacket("udp", "")
			if err != nil {
				logf("UDP remote listen error: %v", err)
				continue
			}

			nm.Add(raddr, c, pc)
		}

		_, err = pc.WriteTo(payload, tgtUDPAddr) // accept only UDPAddr despite the signature
		if err != nil {
			logf("UDP remote write error: %v", err)
			continue
		}
	}
}

// Packet NAT table
type natmap struct {
	sync.RWMutex
	m       map[string]net.PacketConn
	timeout time.Duration
}

func newNATmap(timeout time.Duration) *natmap {
	m := &natmap{}
	m.m = make(map[string]net.PacketConn)
	m.timeout = timeout
	return m
}

func (m *natmap) Get(key string) net.PacketConn {
	m.RLock()
	defer m.RUnlock()
	return m.m[key]
}

func (m *natmap) Set(key string, pc net.PacketConn) {
	m.Lock()
	defer m.Unlock()

	m.m[key] = pc
}

func (m *natmap) Del(key string) net.PacketConn {
	m.Lock()
	defer m.Unlock()

	pc, ok := m.m[key]
	if ok {
		delete(m.m, key)
		return pc
	}
	return nil
}

func (m *natmap) Add(peer net.Addr, dst, src net.PacketConn) {
	m.Set(peer.String(), src)

	go func() {
		timedCopy(dst, peer, src, m.timeout)
		if pc := m.Del(peer.String()); pc != nil {
			pc.Close()
		}
	}()
}

// copy from src to dst with addr with read timeout
func timedCopy(dst net.PacketConn, addr net.Addr, src net.PacketConn, timeout time.Duration) error {
	buf := make([]byte, udpBufSize)

	for {
		src.SetReadDeadline(time.Now().Add(timeout))
		n, _, err := src.ReadFrom(buf)
		if err != nil {
			return err
		}

		_, err = dst.WriteTo(buf[:n], addr)
		if err != nil {
			return err
		}
	}
}
