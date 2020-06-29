package nebula

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const nicID tcpip.NICID = 1

var once sync.Once

type httpTransport struct {
	cidr  *net.IPNet
	mtu   int
	nic   *channel.Endpoint
	stack *stack.Stack

	io.ReadWriteCloser
	client net.Conn
}

func newHTTPTransport(cidr *net.IPNet, mtu int) (*httpTransport, error) {
	// the math/rand package is used for ephemeral port selection
	// in the gvisor stack.
	once.Do(func() { rand.Seed(time.Now().UnixNano()) })

	// Create the stack with ipv4 and tcp protocols, then add a
	// NIC and ipv4 address.
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocol{ipv4.NewProtocol()},
		TransportProtocols: []stack.TransportProtocol{tcp.NewProtocol()},
	})

	channelEP := channel.New(1024, uint32(mtu), "")
	//if err := s.CreateNIC(1, sniffer.New(channelEP)); err != nil {
	if err := s.CreateNIC(nicID, channelEP); err != nil {
		return nil, fmt.Errorf("could not create virtual NIC: %s", err)
	}

	addr := tcpip.Address(cidr.IP.To4())
	if err := s.AddAddress(nicID, ipv4.ProtocolNumber, addr); err != nil {
		return nil, fmt.Errorf("could not add local IP addr %s to NIC: %s", addr, err)
	}

	// Add default route.
	s.SetRouteTable([]tcpip.Route{
		{
			Destination: header.IPv4EmptySubnet,
			NIC:         nicID,
		},
	})

	nebula, client := net.Pipe()
	return &httpTransport{
		cidr:            cidr,
		mtu:             mtu,
		nic:             channelEP,
		stack:           s,
		client:          client,
		ReadWriteCloser: nebula,
	}, nil
}

func (h *httpTransport) Activate() error {
	return nil
}

func (h *httpTransport) CidrNet() *net.IPNet {
	return h.cidr
}

func (h *httpTransport) DeviceName() string {
	return "virtual"
}

func (h *httpTransport) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)

	host, portString, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid address: %s", err)
	}

	remote := tcpip.FullAddress{
		NIC:  nicID,
		Addr: tcpip.Address(net.ParseIP(host).To4()),
	}

	port, err := strconv.Atoi(portString)
	if err != nil {
		return nil, fmt.Errorf("could not parse remote port from %q %s", portString, err)
	}
	remote.Port = uint16(port)

	// Create network pipe, one end will be returned
	inside, outside := net.Pipe()

	// Create TCP endpoint.
	var wq waiter.Queue
	ep, ipErr := h.stack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if ipErr != nil {
		return nil, fmt.Errorf("could not create remote endpoint: %s", ipErr)
	}

	// We need to handle packet shuffling between the virtual NIC and the nebula network
	go func() {
		// raw packets emitted by the virtual NIC; forward to nebula.
		for {
			pi, ok := h.nic.ReadContext(ctx)
			if !ok {
				// context canceled
				break
			}
			header := pi.Pkt.Header.View()
			payload := pi.Pkt.Data.ToView()

			_, err := h.client.Write(append(header, payload...))
			if err != nil {
				l.WithError(err).Warnln("failed to write packet to nebula network")
				continue
			}
		}
	}()

	go func() {
		// raw packets received from nebula; copy to NIC and http client
		for {
			buf := make([]byte, h.mtu)
			n, err := h.client.Read(buf)
			if err != nil {
				if nerr, ok := err.(net.Error); ok {
					if nerr.Timeout() {
						h.client.SetReadDeadline(time.Time{})
						break
					}
				}

				l.WithError(err).Warnln("could not read packet from nebula network")
				continue
			}
			pb := &stack.PacketBuffer{
				Data: buffer.View(buf[:n]).ToVectorisedView(),
			}

			// Give the original packet back to the virtual NIC to maintain
			// TCP connection state and strip TCP/IP headers.
			h.nic.InjectInbound(ipv4.ProtocolNumber, pb)

			// Send a copy of the remaining payload to the http client
			_, err = inside.Write(pb.Data.ToView())
			if err != nil {
				l.WithError(err).Warnln("could not return packet to http client")
				continue
			}
		}
	}()

	go func() {
		// outbound payloads from the http client; ask NIC to send
		for {
			buf := make([]byte, h.mtu)
			n, err := inside.Read(buf)
			if err == io.EOF {
				break
			}
			if err != nil {
				l.WithError(err).Warnln("error reading from http client")
				continue
			}
			_, _, terr := ep.Write(tcpip.SlicePayload(buf[:n]), tcpip.WriteOptions{})
			if terr != nil {
				l.WithError(fmt.Errorf("%s", terr)).Warnln("could not deliver payload to remote endpoint")
				continue
			}
		}
	}()

	// Issue connect request and wait for it to complete.
	waitEntry, notifyCh := waiter.NewChannelEntry(nil)
	wq.EventRegister(&waitEntry, waiter.EventOut)
	terr := ep.Connect(remote)
	if terr == tcpip.ErrConnectStarted {
		//fmt.Println("Connect is pending...")
		<-notifyCh
		terr = ep.GetSockOpt(tcpip.ErrorOption{})
	}
	wq.EventUnregister(&waitEntry)

	if terr != nil {
		return nil, fmt.Errorf("failed to connect to host: %s", terr)
	}

	//fmt.Println("Connected")

	return &pipeCloser{
		Conn:   outside,
		client: h.client,
		cancel: cancel,
	}, nil
}

func (h *httpTransport) WriteRaw(b []byte) error {
	_, err := h.Write(b)
	return err
}

type pipeCloser struct {
	net.Conn

	cancel func()
	client net.Conn
}

func (p *pipeCloser) Close() error {
	p.cancel()
	p.client.SetReadDeadline(time.Now())
	return p.Conn.Close()
}
