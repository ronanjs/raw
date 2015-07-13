// +build linux

package raw

import (
	"errors"
	"net"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/context"
)

var (
	// Must implement net.PacketConn at compile-time.
	_ net.PacketConn = &packetConn{}
)

// packetConn is the Linux-specific implementation of net.PacketConn for this
// package.
type packetConn struct {
	ifi  *net.Interface
	sock int

	// Timeouts set via Set{Read,Write,}Deadline, guarded by mutex
	timeoutMu sync.RWMutex
	rtimeout  time.Time
	wtimeout  time.Time
}

// listenPacket creates a net.PacketConn which can be used to send and receive
// data at the device driver level.
//
// ifi specifies the network interface which will be used to send and receive
// data.  socket specifies the socket type to be used, such as syscall.SOCK_RAW
// or syscall.SOCK_DGRAM.  proto specifies the protocol which should be
// captured and transmitted.  proto is automatically converted to network byte
// order (big endian), akin to the htons() function in C.
func listenPacket(ifi *net.Interface, socket int, proto int) (*packetConn, error) {
	// Convert proto to big endian
	pbe := htons(uint16(proto))

	// Open a packet socket using specified socket and protocol types
	sock, err := syscall.Socket(syscall.AF_PACKET, socket, int(pbe))
	if err != nil {
		return nil, err
	}

	if err := syscall.SetNonblock(sock, true); err != nil {
		return nil, err
	}

	// Bind the packet socket to the interface specified by ifi
	// packet(7):
	//   Only the sll_protocol and the sll_ifindex address fields are used for
	//   purposes of binding.
	err = syscall.Bind(sock, &syscall.SockaddrLinklayer{
		Protocol: pbe,
		Ifindex:  ifi.Index,
	})

	return &packetConn{
		ifi:  ifi,
		sock: sock,
	}, err
}

// ReadFrom implements the net.PacketConn.ReadFrom method.
func (p *packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	// Set up deadline context if needed, if a read timeout is set
	ctx, cancel := context.TODO(), func() {}
	p.timeoutMu.RLock()
	if p.rtimeout.After(time.Now()) {
		ctx, cancel = context.WithDeadline(context.Background(), p.rtimeout)
	}
	p.timeoutMu.RUnlock()

	// Information returned by syscall.Recvfrom
	var n int
	var addr syscall.Sockaddr
	var err error

	for {
		// Continue looping, or if deadline is set and has expired, return
		// an error
		select {
		case <-ctx.Done():
			// We only know how to handle deadline exceeded, so return any
			// other errors for the caller to deal with
			if err := ctx.Err(); err != context.DeadlineExceeded {
				return n, nil, err
			}

			// Return standard net.OpError so caller can detect timeouts and retry
			return n, nil, &net.OpError{
				Op:   "read",
				Net:  "raw",
				Addr: nil,
				Err:  &timeoutError{},
			}
		default:
			// Not timed out, keep trying
		}

		// Attempt to receive on socket
		n, addr, err = syscall.Recvfrom(p.sock, b, 0)
		if err != nil {
			n = 0

			// EAGAIN is returned when no data is available for non-blocking
			// I/O, so keep trying after a short delay
			if err == syscall.EAGAIN {
				time.Sleep(2 * time.Millisecond)
				continue
			}

			// Return other errors
			return n, nil, err
		}

		// Got data, cancel the deadline
		cancel()
		break
	}

	// Retrieve hardware address and other information from addr
	sa, ok := addr.(*syscall.SockaddrLinklayer)
	if !ok {
		return n, nil, errors.New("invalid sockaddr_ll")
	}

	if sa.Halen < 6 {
		return n, nil, errors.New("invalid hardware address")
	}

	// Use length specified to convert byte array into a hardware address slice
	mac := make(net.HardwareAddr, sa.Halen)
	copy(mac, sa.Addr[:])

	// packet(7):
	//   sll_hatype and sll_pkttype are set on received packets for your
	//   information.
	// TODO(mdlayher): determine if similar fields exist and are useful on
	// non-Linux platforms
	return n, &Addr{
		HardwareAddr: mac,
	}, nil
}

// WriteTo implements the net.PacketConn.WriteTo method.
//
// TODO(mdlayher): implement write deadlines
func (p *packetConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	// Ensure correct Addr type
	a, ok := addr.(*Addr)
	if !ok {
		return 0, syscall.EINVAL
	}

	// Convert hardware address back to byte array form
	var baddr [8]byte
	copy(baddr[:], a.HardwareAddr)

	// Send message on socket to the specified hardware address from addr
	// packet(7):
	//   When you send packets it is enough to specify sll_family, sll_addr,
	//   sll_halen, sll_ifindex.  The other fields should  be 0.
	// In this case, sll_family is taken care of automatically by syscall
	err := syscall.Sendto(p.sock, b, 0, &syscall.SockaddrLinklayer{
		Ifindex: p.ifi.Index,
		Halen:   uint8(len(a.HardwareAddr)),
		Addr:    baddr,
	})
	return len(b), err
}

// Close closes the connection.
func (p *packetConn) Close() error {
	return syscall.Close(p.sock)
}

// LocalAddr returns the local network address.
func (p *packetConn) LocalAddr() net.Addr {
	return &Addr{
		HardwareAddr: p.ifi.HardwareAddr,
	}
}

// TODO(mdlayher): it is unfortunate that we have to implement deadlines using
// a context, but it appears that there may not be a better solution until
// Go 1.6 or later.  See here: https://github.com/golang/go/issues/10565.

// SetDeadline implements the net.PacketConn.SetDeadline method.
func (p *packetConn) SetDeadline(t time.Time) error {
	p.timeoutMu.Lock()
	p.rtimeout = t
	p.wtimeout = t
	p.timeoutMu.Unlock()

	return nil
}

// SetReadDeadline implements the net.PacketConn.SetReadDeadline method.
func (p *packetConn) SetReadDeadline(t time.Time) error {
	p.timeoutMu.Lock()
	p.rtimeout = t
	p.timeoutMu.Unlock()

	return nil
}

// SetWriteDeadline implements the net.PacketConn.SetWriteDeadline method.
func (p *packetConn) SetWriteDeadline(t time.Time) error {
	return ErrNotImplemented
}
