package redirect

import (
	"encoding/binary"
	"errors"
	"github.com/tobyxdd/hysteria/pkg/core"
	"github.com/tobyxdd/hysteria/pkg/utils"
	"net"
	"syscall"
	"time"
	"unsafe"
)

const (
	SO_ORIGINAL_DST      = 80
	IP6T_SO_ORIGINAL_DST = 80
)

type TCPRedirect struct {
	HyClient   *core.Client
	ListenAddr *net.TCPAddr
	Timeout    time.Duration

	ConnFunc  func(addr, reqAddr net.Addr)
	ErrorFunc func(addr, reqAddr net.Addr, err error)
}

func NewTCPRedirect(hyClient *core.Client, listen string, timeout time.Duration,
	connFunc func(addr, reqAddr net.Addr),
	errorFunc func(addr, reqAddr net.Addr, err error)) (*TCPRedirect, error) {
	tAddr, err := net.ResolveTCPAddr("tcp", listen)
	if err != nil {
		return nil, err
	}
	r := &TCPRedirect{
		HyClient:   hyClient,
		ListenAddr: tAddr,
		Timeout:    timeout,
		ConnFunc:   connFunc,
		ErrorFunc:  errorFunc,
	}
	return r, nil
}

func (r *TCPRedirect) ListenAndServe() error {
	listener, err := net.ListenTCP("tcp", r.ListenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	for {
		c, err := listener.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			dest, err := getDestAddr(c.(*net.TCPConn))
			if err != nil || dest.IP.IsLoopback() {
				// Silently drop the connection if we failed to get the destination address,
				// or if it's a loopback address (not a redirected connection).
				return
			}
			r.ConnFunc(c.RemoteAddr(), dest)
			rc, err := r.HyClient.DialTCP(dest.String())
			if err != nil {
				r.ErrorFunc(c.RemoteAddr(), dest, err)
				return
			}
			defer rc.Close()
			err = utils.PipePairWithTimeout(c, rc, r.Timeout)
			r.ErrorFunc(c.RemoteAddr(), dest, err)
		}()
	}
}

type sockAddr struct {
	family uint16
	port   [2]byte  // big endian regardless of host byte order
	data   [24]byte // check sockaddr_in or sockaddr_in6 for more information
}

func getDestAddr(conn *net.TCPConn) (*net.TCPAddr, error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var addr sockAddr
	addrSize := uint32(unsafe.Sizeof(addr))
	var err2 error
	err = rc.Control(func(fd uintptr) {
		// try IPv6 first
		_, _, err := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.SOL_IPV6, IP6T_SO_ORIGINAL_DST,
			uintptr(unsafe.Pointer(&addr)), uintptr(unsafe.Pointer(&addrSize)), 0)
		if err != 0 {
			// try IPv4
			_, _, err = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.SOL_IP, SO_ORIGINAL_DST,
				uintptr(unsafe.Pointer(&addr)), uintptr(unsafe.Pointer(&addrSize)), 0)
			if err != 0 {
				// failed
				err2 = err
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if err2 != nil {
		return nil, err2
	}
	switch addr.family {
	case syscall.AF_INET:
		return &net.TCPAddr{IP: addr.data[:4], Port: int(binary.BigEndian.Uint16(addr.port[:]))}, nil
	case syscall.AF_INET6:
		return &net.TCPAddr{IP: addr.data[4:20], Port: int(binary.BigEndian.Uint16(addr.port[:]))}, nil
	default:
		return nil, errors.New("unknown address family")
	}
}