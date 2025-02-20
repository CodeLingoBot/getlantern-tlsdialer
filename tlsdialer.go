// package tlsdialer contains a customized version of crypto/tls.Dial that
// allows control over whether or not to send the ServerName extension in the
// client handshake.
package tlsdialer

import (
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"github.com/getlantern/golog"
	"github.com/getlantern/mtime"
	"github.com/getlantern/netx"
	"github.com/getlantern/ops"

	"github.com/refraction-networking/utls"
)

var (
	log = golog.LoggerFor("tlsdialer")
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "tlsdialer: DialWithDialer timed out" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// Dialer is a configurable dialer that dials using tls
type Dialer struct {
	DoDial         func(net string, addr string, timeout time.Duration) (net.Conn, error)
	Timeout        time.Duration
	Network        string
	SendServerName bool
	ClientHelloID  tls.ClientHelloID
	Config         *tls.Config
}

// A tls.Conn along with timings for key steps in establishing that Conn
type ConnWithTimings struct {
	// Conn: the conn resulting from dialing
	Conn *tls.Conn
	// ResolutionTime: the amount of time it took to resolve the address
	ResolutionTime time.Duration
	// ConnectTime: the amount of time that it took to connect the socket
	ConnectTime time.Duration
	// HandshakeTime: the amount of time that it took to complete the TLS
	// handshake
	HandshakeTime time.Duration
	// ResolvedAddr: the address to which our dns lookup resolved
	ResolvedAddr *net.TCPAddr
	// VerifiedChains: like tls.ConnectionState.VerifiedChains
	VerifiedChains [][]*x509.Certificate
}

// Like crypto/tls.Dial, but with the ability to control whether or not to
// send the ServerName extension in client handshakes through the sendServerName
// flag.
//
// Note - if sendServerName is false, the VerifiedChains field on the
// connection's ConnectionState will never get populated. Use DialForTimings to
// get back a data structure that includes the verified chains.
func Dial(network, addr string, sendServerName bool, config *tls.Config) (*tls.Conn, error) {
	return DialTimeout(netx.DialTimeout, 1*time.Minute, network, addr, sendServerName, config)
}

// Like Dial, but timing out after the given timeout.
func DialTimeout(dial func(net string, addr string, timeout time.Duration) (net.Conn, error), timeout time.Duration, network, addr string, sendServerName bool, config *tls.Config) (*tls.Conn, error) {
	d := &Dialer{
		DoDial:         dial,
		Timeout:        timeout,
		SendServerName: sendServerName,
		Config:         config,
	}
	return d.Dial(network, addr)
}

// Like DialWithDialer but returns a data structure including timings and the
// verified chains.
func DialForTimings(dial func(net string, addr string, timeout time.Duration) (net.Conn, error), timeout time.Duration, network, addr string, sendServerName bool, config *tls.Config) (*ConnWithTimings, error) {
	d := &Dialer{
		DoDial:         dial,
		Timeout:        timeout,
		SendServerName: sendServerName,
		Config:         config,
	}
	return d.DialForTimings(network, addr)
}

// Dial dials the given network and address.
func (d *Dialer) Dial(network, addr string) (*tls.Conn, error) {
	cwt, err := d.DialForTimings(network, addr)
	if err != nil {
		return nil, err
	}
	return cwt.Conn, nil
}

// DialForTimings dials the given network and address and returns a ConnWithTimings.
func (d *Dialer) DialForTimings(network, addr string) (*ConnWithTimings, error) {
	result := &ConnWithTimings{}

	var errCh chan error

	if d.Timeout != 0 {
		// We want the Timeout and Deadline values to cover the whole process: TCP
		// connection and TLS handshake. This means that we also need to start our own
		// timers now.
		errCh = make(chan error, 10)
		time.AfterFunc(d.Timeout, func() {
			errCh <- timeoutError{}
		})
	}

	log.Tracef("Resolving addr: %s", addr)
	elapsed := mtime.Stopwatch()
	var err error
	if d.Timeout == 0 {
		log.Tracef("Resolving immediately")
		result.ResolvedAddr, err = netx.Resolve("tcp", addr)
	} else {
		log.Tracef("Resolving on goroutine")
		resolvedCh := make(chan *net.TCPAddr, 10)
		ops.Go(func() {
			resolved, resolveErr := netx.Resolve("tcp", addr)
			log.Tracef("Resolution resulted in %s : %s", resolved, resolveErr)
			resolvedCh <- resolved
			errCh <- resolveErr
		})
		err = <-errCh
		if err == nil {
			log.Tracef("No error, looking for resolved")
			result.ResolvedAddr = <-resolvedCh
		}
	}

	if err != nil {
		return result, err
	}
	result.ResolutionTime = elapsed()
	log.Tracef("Resolved addr %s to %s in %s", addr, result.ResolvedAddr, result.ResolutionTime)

	hostname, _, err := net.SplitHostPort(addr)
	if err != nil {
		return result, fmt.Errorf("Unable to split host and port for %v: %v", addr, err)
	}

	log.Tracef("Dialing %s %s (%s)", network, addr, result.ResolvedAddr)
	elapsed = mtime.Stopwatch()
	resolvedAddr := result.ResolvedAddr.String()
	rawConn, err := d.DoDial(network, resolvedAddr, d.Timeout)
	if err != nil {
		return result, err
	}
	result.ConnectTime = elapsed()
	log.Tracef("Dialed in %s", result.ConnectTime)

	config := d.Config
	if config == nil {
		config = &tls.Config{}
	}

	serverName := config.ServerName

	if serverName == "" {
		log.Trace("No ServerName set, inferring from the hostname to which we're connecting")
		serverName = hostname
	}
	log.Tracef("ServerName is: %s", serverName)

	log.Trace("Copying config so that we can tweak it")
	configCopy := new(tls.Config)
	*configCopy = *config

	if d.SendServerName {
		log.Tracef("Setting ServerName to %s and relying on the usual logic in tls.Conn.Handshake() to do verification", serverName)
		configCopy.ServerName = serverName
	} else {
		log.Trace("Clearing ServerName and disabling verification in tls.Conn.Handshake(). We'll verify manually after handshaking.")
		configCopy.ServerName = ""
		configCopy.InsecureSkipVerify = true
	}

	chid := d.ClientHelloID
	if chid.Browser == "" {
		log.Trace("Defaulting to typical Golang client hello")
		chid = tls.HelloGolang
	}
	conn := tls.UClient(rawConn, configCopy, chid)

	elapsed = mtime.Stopwatch()
	if d.Timeout == 0 {
		log.Trace("Handshaking immediately")
		err = conn.Handshake()
	} else {
		log.Trace("Handshaking on goroutine")
		ops.Go(func() {
			errCh <- conn.Handshake()
		})
		err = <-errCh
	}
	if err == nil {
		result.HandshakeTime = elapsed()
	}
	log.Tracef("Finished handshaking in: %s", result.HandshakeTime)

	if err == nil && !config.InsecureSkipVerify {
		if d.SendServerName {
			log.Trace("Depending on certificate verification in tls.Conn.Handshake()")
			result.VerifiedChains = conn.ConnectionState().VerifiedChains
		} else {
			log.Trace("Manually verifying certificates")
			configCopy.ServerName = ""
			result.VerifiedChains, err = verifyServerCerts(conn.Conn, serverName, configCopy)
		}
	}

	if err != nil {
		log.Tracef("Handshake or verification error, closing underlying connection: %v", err)
		if closeErr := rawConn.Close(); closeErr != nil {
			log.Debugf("Unable to close connection: %v", closeErr)
		}
		log.Errorf("Error establishing TLS connection to %v: %v", rawConn.RemoteAddr(), err)
		return result, err
	}

	result.Conn = conn.Conn
	return result, nil
}

func verifyServerCerts(conn *tls.Conn, serverName string, config *tls.Config) ([][]*x509.Certificate, error) {
	certs := conn.ConnectionState().PeerCertificates

	opts := x509.VerifyOptions{
		Roots:         config.RootCAs,
		CurrentTime:   time.Now(),
		DNSName:       serverName,
		Intermediates: x509.NewCertPool(),
	}

	for i, cert := range certs {
		if i == 0 {
			continue
		}
		opts.Intermediates.AddCert(cert)
	}
	return certs[0].Verify(opts)
}
