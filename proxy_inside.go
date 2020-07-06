package nebula

import "net"

type mock_inside struct {
	cidr *net.IPNet
}

func (mock_inside) Read([]byte) (int, error) {
	c := make(chan struct{})
	<-c
	return 0, nil
}

func (mock_inside) Write(b []byte) (int, error) {
	return len(b), nil
}

func (mock_inside) Close() error { return nil }

func (mock_inside) Activate() error { return nil }

func (m mock_inside) CidrNet() *net.IPNet { return m.cidr }

func (mock_inside) DeviceName() string { return "proxy" }

func (mock_inside) WriteRaw([]byte) error { return nil }
