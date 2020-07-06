package nebula

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

const ProxyVersion = 1
const ProxyHeaderLen = 13

type ProxyPacketType uint8

const (
	ProxyPacketTypeStart ProxyPacketType = iota
	ProxyPacketTypeOK
	ProxyPacketTypeError
	ProxyPacketTypeMessage
	ProxyPacketTypeBye
)

type proxyServer struct {
	conns proxyConntrack
}

func (f *Interface) Dial(network string, addr string) (net.Conn, error) {
	// TODO: support udp
	if network != "tcp" {
		return nil, fmt.Errorf("only tcp is implemented")
	}

	// TODO: what if addr contains a hostname instead of IP?
	host, portString, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("could not parse address: %s", err)
	}

	ip := net.ParseIP(host)
	port, err := strconv.Atoi(portString)
	if err != nil {
		return nil, fmt.Errorf("could not parse port from address: %s", err)
	}

	client, server := net.Pipe()
	ad := &asyncDialer{
		Conn: server,
		errs: make(chan error, 1),
	}

	pAddr := ProxyConn{
		LocalIP:    ip2int(f.inside.CidrNet().IP.To4()),
		RemoteIP:   ip2int(ip),
		LocalPort:  f.proxy.conns.newPort(),
		RemotePort: uint16(port),
	}

	p := ProxyPacket{
		Type: ProxyPacketTypeStart,
		Addr: pAddr,
	}

	// Store this connection with local/remote reversed as reply packets
	// will be treating us as the remote address.
	if err := f.proxy.conns.Insert(p.ReplyAddr(), ad); err != nil {
		return nil, fmt.Errorf("proxy dial: %s", err)
	}

	p.send(f)

	// TODO: configurable dial timeout
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()

	// TODO: close the conn on error?
	select {
	case <-t.C:
		return nil, fmt.Errorf("remote dial timeout")
	case err := <-ad.errs:
		if err != nil {
			return nil, fmt.Errorf("remote dial failed: %s", err)
		}
	}

	go func() {
		for {
			// TODO: how big should the buffer be?
			buf := make([]byte, 1024)
			n, err := server.Read(buf)
			if err == io.EOF {
				return
			}
			if err != nil {
				// uh oh!
				continue
			}
			if n == 0 {
				continue
			}

			p := ProxyPacket{
				Type:    ProxyPacketTypeMessage,
				Addr:    pAddr,
				Payload: buf[:n],
			}
			p.send(f)
		}
	}()

	return client, nil
}

func (p *proxyServer) HandleInboundPacket(f *Interface, b []byte) {
	packet, err := newProxyPacket(b)
	if err != nil {
		// uh oh!
		panic(err)
	}

	switch packet.Type {
	case ProxyPacketTypeStart:
		l.Println("start packet")
		addr := packet.ReplyAddr()

		// TODO: This blocks until timeout on a bad address, so should probably be in a goroutine
		conn, err := p.conns.Dial(packet.Addr)
		if err != nil {
			// Send an Error packet
			l.WithError(err).Println("start packet oops")
			oops := ProxyPacket{
				Type:    ProxyPacketTypeError,
				Addr:    addr,
				Payload: []byte(fmt.Sprintf("could not dial: %s", err)),
			}
			oops.send(f)
			return
		}

		go func() {
			for {
				// TODO: how much buffer?
				buf := make([]byte, 1200)
				n, err := conn.Read(buf)
				if err == io.EOF {
					return
				}
				if err != nil {
					l.WithError(err).Warn("Error reading on proxy outbound connection")
				}

				if n == 0 {
					continue
				}

				// re-package and forward the payload
				resp := ProxyPacket{
					Type:    ProxyPacketTypeMessage,
					Addr:    addr,
					Payload: buf[:n],
				}

				// encrypt, add nebula headers, write back to original host (using nebula IP)
				resp.send(f)
			}
		}()

		// Send an OK packet
		ok := ProxyPacket{
			Type: ProxyPacketTypeOK,
			Addr: addr,
		}
		ok.send(f)

	case ProxyPacketTypeOK:
		l.Println("ok packet")
		conn, err := p.conns.ConnFor(packet.Addr)
		if err != nil {
			// uh oh!
			panic(err)
		}
		c, ok := conn.(*asyncDialer)
		if !ok {
			// uh oh!
			panic("got an OK but no asyncDialer conn")
		}

		// non-blocking write to channel to indicate that the conn
		// can be released to the client.
		select {
		case c.errs <- nil:
		default:
		}

	case ProxyPacketTypeError:
		l.Println("error packet")
		conn, err := p.conns.ConnFor(packet.Addr)
		if err != nil {
			// uh oh!
			panic(err)
		}
		c, ok := conn.(*asyncDialer)
		if !ok {
			// uh oh?
			panic("got an error for a non-pending connection")
		}

		// TODO: close the conn too?
		select {
		case c.errs <- fmt.Errorf("remote node sent error: %s", packet.Payload):
		default:
		}

	case ProxyPacketTypeMessage:
		l.Println("message packet")
		conn, err := p.conns.ConnFor(packet.Addr)
		if err != nil {
			// uh oh!
			panic(err)
		}
		n, err := conn.Write(packet.Payload)
		if err != nil {
			// uh oh!
			panic(err)
		}
		if n != len(packet.Payload) {
			// aww man! ... uh oh!
			panic(err)
		}

	case ProxyPacketTypeBye:
		l.Println("bye packet")
		if err := p.conns.Close(packet.Addr); err != nil {
			// uh oh!
			panic(err)
		}

	default:
		// uh oh!
	}
}

type ProxyConn struct {
	LocalIP    uint32
	RemoteIP   uint32
	LocalPort  uint16
	RemotePort uint16
}

type ProxyPacket struct {
	Type    ProxyPacketType
	Addr    ProxyConn
	Payload []byte
}

func newProxyPacket(b []byte) (*ProxyPacket, error) {
	if len(b) < ProxyHeaderLen {
		return nil, eHeaderTooShort
	}

	if v := uint8((b[0] >> 4) & 0x0f); v != ProxyVersion {
		return nil, fmt.Errorf("unsupported proxy packet version")
	}

	p := new(ProxyPacket)
	p.Type = ProxyPacketType(b[0] & 0x0f)
	p.Addr.LocalIP = binary.BigEndian.Uint32(b[1:5])
	p.Addr.RemoteIP = binary.BigEndian.Uint32(b[5:9])
	p.Addr.LocalPort = binary.BigEndian.Uint16(b[9:11])
	p.Addr.RemotePort = binary.BigEndian.Uint16(b[11:13])
	p.Payload = b[13:]

	return p, nil
}

func (p ProxyPacket) Bytes() []byte {
	buf := make([]byte, ProxyHeaderLen+len(p.Payload))
	buf[0] = byte(ProxyVersion<<4 | (p.Type & 0x0f))
	binary.BigEndian.PutUint32(buf[1:5], p.Addr.LocalIP)
	binary.BigEndian.PutUint32(buf[5:9], p.Addr.RemoteIP)
	binary.BigEndian.PutUint16(buf[9:11], p.Addr.LocalPort)
	binary.BigEndian.PutUint16(buf[11:13], p.Addr.RemotePort)
	copy(buf[ProxyHeaderLen:], p.Payload)
	return buf
}

func (p ProxyPacket) ReplyAddr() ProxyConn {
	return ProxyConn{
		LocalIP:    p.Addr.RemoteIP,
		RemoteIP:   p.Addr.LocalIP,
		LocalPort:  p.Addr.RemotePort,
		RemotePort: p.Addr.LocalPort,
	}
}

func (p ProxyPacket) send(f *Interface) {
	packet := p.Bytes()
	hostinfo := f.getOrHandshake(p.Addr.RemoteIP)
	ci := hostinfo.ConnectionState

	// TODO: figure out how the cache works and implement cached proxy packets
	// if ci.ready == false {
	// 	// Because we might be sending stored packets, lock here to stop new things going to
	// 	// the packet queue.
	// 	ci.queueLock.Lock()
	// 	if !ci.ready {
	// 		hostinfo.cachePacket(proxy, 0, packet, f.sendMessageNow)
	// 		ci.queueLock.Unlock()
	// 		return
	// 	}
	// 	ci.queueLock.Unlock()
	// }

	dropReason := error(nil) //TODO: f.firewall.Drop(packet, *fwPacket, false, hostinfo, trustedCAs)
	if dropReason == nil {
		out := make([]byte, mtu)
		nb := make([]byte, 12, 12)
		f.sendNoMetrics(proxy, 0, ci, hostinfo, hostinfo.remote, packet, nb, out)
		if f.lightHouse != nil && *ci.messageCounter%5000 == 0 {
			f.lightHouse.Query(p.Addr.RemoteIP, f)
		}
	} else if l.Level >= logrus.DebugLevel {
		hostinfo.logger().
			//WithField("fwPacket", fwPacket).
			WithField("reason", dropReason).
			Debugln("dropping outbound packet")
	}
}

type asyncDialer struct {
	net.Conn
	errs chan error
}

type proxyConntrack struct {
	mu sync.RWMutex
	m  map[ProxyConn]net.Conn

	// TODO: This should probably be a map tracking port use, so we can cycle back around safely.
	ephemeral uint32
}

func (p *proxyConntrack) Dial(addr ProxyConn) (net.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.m == nil {
		p.m = make(map[ProxyConn]net.Conn)
	}

	if _, ok := p.m[addr]; ok {
		return nil, fmt.Errorf("connection already established")
	}

	// TODO: support udp too?
	c, err := net.Dial("tcp", fmt.Sprintf("%s:%d", int2ip(addr.RemoteIP), addr.RemotePort))
	if err != nil {
		l.WithError(err).Println("nope")
		return nil, fmt.Errorf("could not dial %s:%d: %s", int2ip(addr.RemoteIP), addr.RemotePort, err)
	}

	p.m[addr] = c
	return c, nil
}

func (p *proxyConntrack) ConnFor(addr ProxyConn) (net.Conn, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	c, ok := p.m[addr]
	if !ok {
		return nil, fmt.Errorf("no such connection")
	}

	return c, nil
}

func (p *proxyConntrack) Close(addr ProxyConn) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	c, ok := p.m[addr]
	if !ok {
		return fmt.Errorf("connection already closed")
	}

	delete(p.m, addr)
	return c.Close()
}

func (p *proxyConntrack) Insert(addr ProxyConn, conn net.Conn) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.m == nil {
		p.m = make(map[ProxyConn]net.Conn)
	}

	if _, ok := p.m[addr]; ok {
		return fmt.Errorf("connection already established")
	}

	p.m[addr] = conn
	return nil
}

const ephemeralStart = 49152
const ephemeralEnd = 65500

func (p *proxyConntrack) newPort() uint16 {
	i := atomic.AddUint32(&p.ephemeral, 1)
	if i > ephemeralEnd-ephemeralStart {
		// dangerous cycle around
		p.ephemeral = 0
	}
	return uint16(ephemeralStart + i)
}
