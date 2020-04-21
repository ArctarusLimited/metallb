package bgp // import "go.universe.tf/metallb/internal/bgp"

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"net"
	"os"
	"reflect"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"golang.org/x/sys/unix"
)

var errClosed = errors.New("session closed")

// Session represents one BGP session to an external router.
type Session struct {
	asn              uint32
	routerID         net.IP // May be nil, meaning "derive from context"
	myNode           string
	addr             string
	srcAddr          net.IP
	peerASN          uint32
	peerFBASNSupport bool
	mpIPv4Support    bool
	mpIPv6Support    bool
	holdTime         time.Duration
	logger           log.Logger
	password         string
	newHoldTime      chan bool
	backoff          backoff
	// allows to use MP BGP encoding for IPv4 if supported by peer
	allowMPBGPEncodingV4 bool

	mu               sync.Mutex
	cond             *sync.Cond
	closed           bool
	conn             net.Conn
	actualHoldTime   time.Duration
	defaultNextHopV4 net.IP
	defaultNextHopV6 net.IP
	advertised       map[string]*Advertisement
	new              map[string]*Advertisement
}

// run tries to stay connected to the peer, and pumps route updates to it.
func (s *Session) run() {
	defer stats.DeleteSession(s.addr)
	for {
		if err := s.connect(); err != nil {
			if err == errClosed {
				return
			}
			level.Error(s.logger).Log("op", "connect", "error", err, "msg", "failed to connect to peer")
			backoff := s.backoff.Duration()
			time.Sleep(backoff)
			continue
		}
		stats.SessionUp(s.addr)
		s.backoff.Reset()

		level.Info(s.logger).Log("event", "sessionUp", "msg", "BGP session established")

		if !s.sendUpdates() {
			return
		}
		stats.SessionDown(s.addr)
		level.Warn(s.logger).Log("event", "sessionDown", "msg", "BGP session down")
	}
}

// sendUpdates waits for changes to desired advertisements, and pushes
// them out to the peer.
func (s *Session) sendUpdates() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return false
	}
	if s.conn == nil {
		return true
	}
	s.handleNewAdvertisements()
	if s.new != nil {
		s.advertised, s.new = s.new, nil
	}

	for _, adv := range s.advertised {
		if ret := s.sendUpdateForAdvertisement(adv); ret {
			return ret
		}
	}
	stats.AdvertisedPrefixes(s.addr, len(s.advertised))

	for {
		for s.new == nil && s.conn != nil {
			s.cond.Wait()
		}

		if s.closed {
			return false
		}
		if s.conn == nil {
			return true
		}
		s.handleNewAdvertisements()
		if s.new == nil {
			// nil is "no pending updates", contrast to a non-nil
			// empty map which means "withdraw all".
			continue
		}
		for c, adv := range s.new {
			if adv2, ok := s.advertised[c]; ok && adv.Equal(adv2) {
				// Peer already has correct state for this
				// advertisement, nothing to do.
				continue
			}
			if ret := s.sendUpdateForAdvertisement(adv); ret {
				return ret
			}
		}

		wdr := []*net.IPNet{}
		for c, adv := range s.advertised {
			if s.new[c] == nil {
				wdr = append(wdr, adv.Prefix)
			}
		}
		if len(wdr) > 0 {
			if err := sendWithdraw(s.conn, s.useMPBGPforV4(), wdr); err != nil {
				s.abort()
				for _, pfx := range wdr {
					level.Error(s.logger).Log("op", "sendWithdraw", "prefix", pfx, "error", err, "msg", "failed to send BGP withdraw")
				}
				return true
			}
			stats.UpdateSent(s.addr)
		}
		s.advertised, s.new = s.new, nil
		stats.AdvertisedPrefixes(s.addr, len(s.advertised))
	}
}

// remove Advertisements which can't be announced
func (s *Session) handleNewAdvertisements() {
	if len(s.new) == 0 || s.new == nil {
		return
	}
	for c, adv := range s.new {
		s.updateNextHop(adv)
		if adv.NextHop == nil {
			level.Warn(s.logger).Log("op", "SetAdvertisement", "prefix", c,
				"msg", "skip prefix announcement because there is no next hop for it")
			delete(s.new, c)
		}
		if adv.Prefix.IP.To4() == nil && !s.mpIPv6Support {
			level.Warn(s.logger).Log("op", "SetAdvertisement", "prefix", c,
				"msg", "skip prefix announcement because MP BGP for IPv6 is not supported by peer")
			delete(s.new, c)
		}
	}
	if len(s.new) == 0 {
		s.new = nil
		stats.PendingPrefixes(s.addr, 0)
	}
}

func (s *Session) sendUpdateForAdvertisement(adv *Advertisement) bool {
	if err := sendUpdate(s.conn, s.asn, s.isIBGP(), s.peerFBASNSupport, s.useMPBGP(adv), adv); err != nil {
		s.abort()
		level.Error(s.logger).Log("op", "sendUpdate", "prefix", adv.Prefix.String(),
			"error", err, "msg", "failed to send BGP update")
		return true
	}
	stats.UpdateSent(s.addr)
	return false
}

func (s *Session) isIBGP() bool {
	return s.asn == s.peerASN
}

func (s *Session) updateNextHop(adv *Advertisement) {
	if adv.NextHop != nil {
		return
	}
	if adv.Prefix.IP.To4() == nil {
		adv.NextHop = s.defaultNextHopV6
		return
	}
	adv.NextHop = s.defaultNextHopV4
}

// we should use MP BGP encoding in case:
// - ipv6 announce
// - if MP BGP encoding enabled for peer via config and peer supports it (sends advertise MP extension for ipv4)
func (s *Session) useMPBGP(adv *Advertisement) bool {
	return s.useMPBGPforV4() || adv.Prefix.IP.To4() == nil
}

func (s *Session) useMPBGPforV4() bool {
	return s.mpIPv4Support && s.allowMPBGPEncodingV4
}

// connect establishes the BGP session with the peer.
// Sets TCP_MD5 sockopt if password is !="".
func (s *Session) connect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	conn, err := dialMD5(ctx, s.addr, s.srcAddr, s.password)
	if err != nil {
		return fmt.Errorf("dial %q: %s", s.addr, err)
	}

	if err = conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return fmt.Errorf("setting deadline on conn to %q: %s", s.addr, err)
	}
	if err := s.getDefaultNextHops(conn); err != nil {
		return err
	}
	routerID := s.routerID
	if routerID == nil {
		routerID, err = getRouterID(s.defaultNextHopV4, s.myNode)
		if err != nil {
			return err
		}
	}

	if err = sendOpen(conn, s.asn, routerID, s.holdTime); err != nil {
		conn.Close()
		return fmt.Errorf("send OPEN to %q: %s", s.addr, err)
	}

	op, err := readOpen(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("read OPEN from %q: %s", s.addr, err)
	}
	if op.asn != s.peerASN {
		conn.Close()
		return fmt.Errorf("unexpected peer ASN %d, want %d", op.asn, s.peerASN)
	}
	s.peerFBASNSupport = op.fbasn
	if s.asn > 65536 && !s.peerFBASNSupport {
		conn.Close()
		return fmt.Errorf("peer does not support 4-byte ASNs")
	}
	s.mpIPv4Support = op.mp4
	s.mpIPv6Support = op.mp6

	// BGP session is established, clear the connect timeout deadline.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return fmt.Errorf("clearing deadline on conn to %q: %s", s.addr, err)
	}

	// Consume BGP messages until the connection closes.
	go s.consumeBGP(conn)

	// Send one keepalive to say that yes, we accept the OPEN.
	if err := sendKeepalive(conn); err != nil {
		conn.Close()
		return fmt.Errorf("accepting peer OPEN from %q: %s", s.addr, err)
	}

	// Set up regular keepalives from now on.
	s.actualHoldTime = s.holdTime
	if op.holdTime < s.actualHoldTime {
		s.actualHoldTime = op.holdTime
	}
	select {
	case s.newHoldTime <- true:
	default:
	}

	s.conn = conn
	return nil
}

func (s *Session) getDefaultNextHops(conn net.Conn) error {
	addr, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		conn.Close()
		return fmt.Errorf("error getting local addr for default nexthop to %q", s.addr)
	}
	if addr.IP.To4() != nil {
		s.defaultNextHopV4 = addr.IP
		s.defaultNextHopV6 = findAltIP(addr.IP)
		if s.defaultNextHopV6 == nil {
			s.logger.Log("op", "connect", "msg", "can't find IPv6 address to use as next hop")
		}
		return nil
	}
	s.defaultNextHopV6 = addr.IP
	s.defaultNextHopV4 = findAltIP(addr.IP)
	if s.defaultNextHopV4 == nil {
		s.logger.Log("op", "connect", "msg", "can't find IPv4 address to use as next hop")
	}
	return nil
}

func hashRouterId(hostname string) (net.IP, error) {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.LittleEndian, crc32.ChecksumIEEE([]byte(hostname)))
	if err != nil {
		return nil, err
	}
	return net.IP(buf.Bytes()), nil
}

// Ipv4 address will be used if it exist
// hash from hostname value will be used as fallback
func getRouterID(addr net.IP, myNode string) (net.IP, error) {
	if addr.To4() != nil {
		return addr, nil
	}
	return hashRouterId(myNode)
}

// if addr is IPv4, will return IPv6 address on the same interface or nil
// if addr is IPv6, will return IPv4 address on the same interface or nil
func findAltIP(addr net.IP) net.IP {
	var findIPv4 bool
	if addr.To4() == nil {
		findIPv4 = true
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip.Equal(addr) {
				// This is the interface.
				// Loop through the addresses again and search for IP
				for _, a := range addrs {
					var ip net.IP
					switch v := a.(type) {
					case *net.IPNet:
						ip = v.IP
					case *net.IPAddr:
						ip = v.IP
					}
					if ip == nil {
						continue
					}
					if findIPv4 {
						if ip.To4() != nil {
							return ip
						}
					} else {
						if ip.To4() == nil && ip.IsGlobalUnicast() {
							return ip
						}
					}
				}
				return nil
			}
		}
	}
	return nil
}

// sendKeepalives sends BGP KEEPALIVE packets at the negotiated rate
// whenever the session is connected.
func (s *Session) sendKeepalives() {
	var (
		t  *time.Ticker
		ch <-chan time.Time
	)

	for {
		select {
		case <-s.newHoldTime:
			s.mu.Lock()
			ht := s.actualHoldTime
			s.mu.Unlock()
			if t != nil {
				t.Stop()
				t = nil
				ch = nil
			}
			if ht != 0 {
				t = time.NewTicker(ht / 3)
				ch = t.C
			}

		case <-ch:
			if err := s.sendKeepalive(); err == errClosed {
				// Session has been closed by package caller, we're
				// done here.
				return
			}
		}
	}
}

// sendKeepalive sends a single BGP KEEPALIVE packet.
func (s *Session) sendKeepalive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	if s.conn == nil {
		// No connection established, othing to do.
		return nil
	}
	if err := sendKeepalive(s.conn); err != nil {
		s.abort()
		level.Error(s.logger).Log("op", "sendKeepalive", "error", err, "msg", "failed to send keepalive")
		return fmt.Errorf("sending keepalive to %q: %s", s.addr, err)
	}
	return nil
}

// New creates a BGP session using the given session parameters.
//
// The session will immediately try to connect and synchronize its
// local state with the peer.
func New(l log.Logger, addr string, srcAddr net.IP, asn uint32, routerID net.IP, peerASN uint32,
	holdTime time.Duration, password string, myNode string, allowMPBGPEncodingV4 bool) (*Session, error) {
	ret := &Session{
		addr:                 addr,
		srcAddr:     srcAddr,
		asn:                  asn,
		routerID:             routerID.To4(),
		myNode:               myNode,
		peerASN:              peerASN,
		holdTime:             holdTime,
		logger:               log.With(l, "peer", addr, "localASN", asn, "peerASN", peerASN),
		newHoldTime:          make(chan bool, 1),
		advertised:           map[string]*Advertisement{},
		password:             password,
		allowMPBGPEncodingV4: allowMPBGPEncodingV4,
	}
	ret.cond = sync.NewCond(&ret.mu)
	go ret.sendKeepalives()
	go ret.run()

	stats.sessionUp.WithLabelValues(ret.addr).Set(0)
	stats.prefixes.WithLabelValues(ret.addr).Set(0)

	return ret, nil
}

// consumeBGP receives BGP messages from the peer, and ignores
// them. It does minimal checks for the well-formedness of messages,
// and terminates the connection if something looks wrong.
func (s *Session) consumeBGP(conn io.ReadCloser) {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.conn == conn {
			s.abort()
		} else {
			conn.Close()
		}
	}()

	for {
		hdr := struct {
			Marker1, Marker2 uint64
			Len              uint16
			Type             uint8
		}{}
		if err := binary.Read(conn, binary.BigEndian, &hdr); err != nil {
			// TODO: log, or propagate the error somehow.
			return
		}
		if hdr.Marker1 != 0xffffffffffffffff || hdr.Marker2 != 0xffffffffffffffff {
			// TODO: propagate
			return
		}
		if hdr.Type == 3 {
			// TODO: propagate better than just logging directly.
			err := readNotification(conn)
			level.Error(s.logger).Log("event", "peerNotification", "error", err, "msg", "peer sent notification, closing session")
			return
		}
		if _, err := io.Copy(ioutil.Discard, io.LimitReader(conn, int64(hdr.Len)-19)); err != nil {
			// TODO: propagate
			return
		}
	}
}

// Set updates the set of Advertisements that this session's peer should receive.
//
// Changes are propagated to the peer asynchronously, Set may return
// before the peer learns about the changes.
func (s *Session) Set(advs ...*Advertisement) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	newAdvs := map[string]*Advertisement{}
	for _, adv := range advs {
		if len(adv.Communities) > 63 {
			return fmt.Errorf("max supported communities is 63, got %d", len(adv.Communities))
		}
		newAdvs[adv.Prefix.String()] = adv
	}

	s.new = newAdvs
	stats.PendingPrefixes(s.addr, len(s.new))
	s.cond.Broadcast()
	return nil
}

// abort closes any existing connection, updates stats, and cleans up
// state ready for another connection attempt.
func (s *Session) abort() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
		stats.SessionDown(s.addr)
	}
	// Next time we retry the connection, we can just skip straight to
	// the desired end state.
	if s.new != nil {
		s.advertised, s.new = s.new, nil
		stats.PendingPrefixes(s.addr, len(s.advertised))
	}
	s.cond.Broadcast()
}

// Close shuts down the BGP session.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.abort()
	return nil
}

// Advertisement represents one network path and its BGP attributes.
type Advertisement struct {
	// The prefix being advertised to the peer.
	Prefix *net.IPNet
	// The address of the router to which the peer should forward traffic.
	NextHop net.IP
	// The local preference of this route. Only propagated to IBGP
	// peers (i.e. where the peer ASN matches the local ASN).
	LocalPref uint32
	// BGP communities to attach to the path.
	Communities []uint32
}

// Equal returns true if a and b are equivalent advertisements.
func (a *Advertisement) Equal(b *Advertisement) bool {
	if a.Prefix.String() != b.Prefix.String() {
		return false
	}
	if !a.NextHop.Equal(b.NextHop) {
		return false
	}
	if a.LocalPref != b.LocalPref {
		return false
	}
	return reflect.DeepEqual(a.Communities, b.Communities)
}

const (
	//tcpMD5SIG TCP MD5 Signature (RFC2385).
	tcpMD5SIG = 14
)

// This  struct is defined at; linux-kernel: include/uapi/linux/tcp.h,
// It  must be kept in sync with that definition, see current version:
// https://github.com/torvalds/linux/blob/v4.16/include/uapi/linux/tcp.h#L253
// nolint[structcheck]
type tcpmd5sig struct {
	ssFamily uint16
	ss       [126]byte
	pad1     uint16
	keylen   uint16
	pad2     uint32
	key      [80]byte
}

// DialTCP does the part of creating a connection manually,  including setting the
// proper TCP MD5 options when the password is not empty. Works by manupulating
// the low level FD's, skipping the net.Conn API as it has not hooks to set
// the neccessary sockopts for TCP MD5.
func dialMD5(ctx context.Context, addr string, srcAddr net.IP, password string) (net.Conn, error) {
	// If srcAddr exists on any of the local network interfaces, use it as the
	// source address of the TCP socket. Otherwise, use the IPv6 unspecified
	// address ("::") to let the kernel figure out the source address.
	// NOTE: On Linux, "::" also includes "0.0.0.0" (all IPv4 addresses).
	a := "[::]"
	if srcAddr != nil {
		ifs, err := net.Interfaces()
		if err != nil {
			return nil, fmt.Errorf("Querying local interfaces: %w", err)
		}

		if !localAddressExists(ifs, srcAddr) {
			return nil, fmt.Errorf("Address %q doesn't exist on this host", srcAddr)
		}

		a = fmt.Sprintf("[%s]", srcAddr.String())
	}

	laddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:0", a))
	if err != nil {
		return nil, fmt.Errorf("Error resolving local address: %s ", err)
	}

	raddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("invalid remote address: %s ", err)
	}

	var family int
	var ra, la unix.Sockaddr
	if raddr.IP.To4() != nil {
		family = unix.AF_INET
		rsockaddr := &unix.SockaddrInet4{Port: raddr.Port}
		copy(rsockaddr.Addr[:], raddr.IP.To4())
		ra = rsockaddr
		lsockaddr := &unix.SockaddrInet4{}
		copy(lsockaddr.Addr[:], laddr.IP.To4())
		la = lsockaddr
	} else {
		family = unix.AF_INET6
		rsockaddr := &unix.SockaddrInet6{Port: raddr.Port}
		copy(rsockaddr.Addr[:], raddr.IP.To16())
		ra = rsockaddr
		var zone uint32
		if laddr.Zone != "" {
			intf, errs := net.InterfaceByName(laddr.Zone)
			if errs != nil {
				return nil, errs
			}
			zone = uint32(intf.Index)
		}
		lsockaddr := &unix.SockaddrInet6{ZoneId: zone}
		copy(lsockaddr.Addr[:], laddr.IP.To16())
		la = lsockaddr
	}

	sockType := unix.SOCK_STREAM | unix.SOCK_CLOEXEC | unix.SOCK_NONBLOCK
	proto := 0
	fd, err := unix.Socket(family, sockType, proto)
	if err != nil {
		return nil, err
	}

	// A new socket was created so we must close it before this
	// function returns either on failure or success. On success,
	// net.FileConn() in newTCPConn() increases the refcount of
	// the socket so this fi.Close() doesn't destroy the socket.
	// The caller must call Close() with the file later.
	// Note that the above os.NewFile() doesn't play with the
	// refcount.
	fi := os.NewFile(uintptr(fd), "")
	defer fi.Close()

	if password != "" {
		sig := buildTCPMD5Sig(raddr.IP, password)
		b := *(*[unsafe.Sizeof(sig)]byte)(unsafe.Pointer(&sig))
		// Better way may be available in  Go 1.11, see go-review.googlesource.com/c/go/+/72810
		if err = os.NewSyscallError("setsockopt", unix.SetsockoptString(fd, unix.IPPROTO_TCP, tcpMD5SIG, string(b[:]))); err != nil {
			return nil, err
		}
	}

	if err = unix.Bind(fd, la); err != nil {
		return nil, os.NewSyscallError("bind", err)
	}

	err = unix.Connect(fd, ra)

	switch err {
	case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
	case nil:
		return net.FileConn(fi)
	default:
		return nil, os.NewSyscallError("connect", err)
	}

	// With a non-blocking socket, the connection process is
	// asynchronous, so we need to manually wait with epoll until the
	// connection succeeds. All of the following is doing that, with
	// appropriate use of the deadline in the context.
	epfd, err := unix.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	defer unix.Close(epfd)

	var event unix.EpollEvent
	events := make([]unix.EpollEvent, 1)

	event.Events = syscall.EPOLLIN | syscall.EPOLLOUT | syscall.EPOLLPRI
	event.Fd = int32(fd)
	if err = unix.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &event); err != nil {
		return nil, err
	}

	for {
		timeout := int(-1)
		if deadline, ok := ctx.Deadline(); ok {
			timeout = int(time.Until(deadline).Nanoseconds() / 1000000)
			if timeout <= 0 {
				return nil, fmt.Errorf("timeout")
			}
		}
		nevents, err := unix.EpollWait(epfd, events, timeout)
		if err != nil {
			return nil, err
		}
		if nevents == 0 {
			return nil, fmt.Errorf("timeout")
		}
		if nevents > 1 || events[0].Fd != int32(fd) {
			return nil, fmt.Errorf("unexpected epoll behavior")
		}

		nerr, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return nil, os.NewSyscallError("getsockopt", err)
		}
		switch err := syscall.Errno(nerr); err {
		case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
		case syscall.Errno(0), unix.EISCONN:
			return net.FileConn(fi)
		default:
			return nil, os.NewSyscallError("getsockopt", err)
		}
	}
}

func buildTCPMD5Sig(addr net.IP, key string) tcpmd5sig {
	t := tcpmd5sig{}
	if addr.To4() != nil {
		t.ssFamily = unix.AF_INET
		copy(t.ss[2:], addr.To4())
	} else {
		t.ssFamily = unix.AF_INET6
		copy(t.ss[6:], addr.To16())
	}

	t.keylen = uint16(len(key))
	copy(t.key[0:], []byte(key))

	return t
}

// localAddressExists returns true if the address addr exists on any of the
// network interfaces in the ifs slice.
func localAddressExists(ifs []net.Interface, addr net.IP) bool {
	for _, i := range ifs {
		addresses, err := i.Addrs()
		if err != nil {
			continue
		}

		for _, a := range addresses {
			ip, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip.IP.Equal(addr) {
				return true
			}
		}
	}

	return false
}
