package autotunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	autotunnelconfig "github.com/kamaln7/autotunnel/internal/config"
	"github.com/kevinburke/ssh_config"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type AutoTunnel struct {
	ctx    context.Context
	cancel context.CancelFunc

	config  *autotunnelconfig.Config
	ll      *logrus.Logger
	monitor *NetworkInterfaceMonitor

	connections map[string]*connection
	tunnels     []*tunnel
	wg          sync.WaitGroup
}

type connection struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	host            *host
	proxy           Proxy
	ll              *logrus.Entry
	dialTimeout     time.Duration
	inactiveTimeout time.Duration

	paused     bool
	pausedLock sync.Mutex

	cl                 *ssh.Client
	clMutex            sync.RWMutex
	active             bool
	lastError          string
	currentConnections uint32
	inactiveTimer      *time.Timer
	connectionsMutex   sync.RWMutex
	isProxy            bool
}

type tunnel struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	name        string
	connection  *connection
	localPort   string
	remotePort  string
	ll          *logrus.Entry
	dialTimeout time.Duration

	listener           net.Listener
	lastError          string
	currentConnections uint32
}

type commandProxy struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	ll              *logrus.Entry
	command         []string
	inactiveTimeout time.Duration

	active             bool
	process            *os.Process
	conn               net.Conn
	mtx                sync.RWMutex
	inactiveTimer      *time.Timer
	currentConnections uint32
}

type Proxy interface {
	Dial(ctx context.Context, ll *logrus.Entry, n, addr string) (net.Conn, error)
	Active() bool
	TrackConnection()
	TrackConnectionClosed()
}

func New(ctx context.Context, c *autotunnelconfig.Config, ll *logrus.Logger) (*AutoTunnel, error) {
	at := &AutoTunnel{
		config:      c,
		ll:          ll,
		connections: make(map[string]*connection),
	}

	at.ctx, at.cancel = context.WithCancel(ctx)
	hosts := make(map[string]*host)
	for i, t := range at.config.Tunnels {
		ll := ll.WithField("tunnel_index", i)
		if t.Name == "" {
			m := "tunnel missing name"
			ll.Error(m)
			return nil, fmt.Errorf("parsing tunnel [%d]: %s", i, m)
		} else {
			ll = ll.WithField("tunnel_name", t.Name)
		}
		if t.Host == "" {
			m := "tunnel missing host"
			ll.Error(m)
			return nil, fmt.Errorf("parsing tunnel %s: %s", t.Name, m)
		}

		if t.LocalPort == "" && t.RemotePort == "" {
			m := "tunnel requires at least one of remote_port or local_port"
			ll.Error(m)
			return nil, fmt.Errorf("parsing tunnel %s: %s", t.Name, m)
		}

		if t.LocalPort == "" && t.RemotePort != "" {
			t.LocalPort = t.RemotePort
		} else if t.RemotePort == "" && t.LocalPort != "" {
			t.RemotePort = t.LocalPort
		}

		hosts[t.Host] = &host{}
		if t.JumpHost != "" {
			hosts[t.JumpHost] = &host{}
		}
	}

	for alias, host := range hosts {
		host.alias = alias
		hostname := ssh_config.Get(alias, "Hostname")
		if hostname == "" {
			hostname = alias
		}
		port := ssh_config.Get(alias, "Port")
		if port == "" {
			port = "22"
		}
		username := ssh_config.Get(alias, "User")
		if username == "" {
			u, err := user.Current()
			if err != nil {
				return nil, err
			}
			username = u.Username
		}

		host.hostport = fmt.Sprintf("%s:%s", hostname, port)
		host.username = username

		conn := &connection{
			host: host,
			ll: at.ll.WithFields(logrus.Fields{
				"host":         alias,
				"dial_timeout": at.config.DialTimeout,
			}),
			wg:              at.wg,
			dialTimeout:     at.config.DialTimeout,
			inactiveTimeout: at.config.InactiveTimeout,
		}
		conn.ctx, conn.cancel = context.WithCancel(at.ctx)
		at.connections[alias] = conn
	}

	for _, t := range at.config.Tunnels {
		if at.connections[t.Host].proxy != nil {
			continue
		}

		var proxy Proxy
		if t.JumpHost != "" {
			p := at.connections[t.JumpHost]
			if !p.isProxy {
				p.isProxy = true
				p.ll = p.ll.WithField("proxy_type", "jump_host")
			}
			proxy = p
		} else if len(t.JumpCommand) > 0 {
			ll.WithField("jump_command", t.JumpCommand).Trace("creating command proxy")
			p := &commandProxy{
				ll: at.ll.WithFields(logrus.Fields{
					"jump_command": t.JumpCommand,
					"proxy_type":   "jump_command",
				}),
				command:         t.JumpCommand,
				inactiveTimeout: at.config.InactiveTimeout,
				wg:              at.wg,
			}
			p.ctx, p.cancel = context.WithCancel(at.ctx)
			proxy = p
		}
		at.connections[t.Host].proxy = proxy
	}

	for _, t := range at.config.Tunnels {
		tt := &tunnel{
			name:       t.Name,
			localPort:  t.LocalPort,
			remotePort: t.RemotePort,
			connection: at.connections[t.Host],
			ll: at.ll.WithFields(logrus.Fields{
				"tunnel":           t.Name,
				"local_port":       t.LocalPort,
				"remote_port":      t.RemotePort,
				"ssh_dial_timeout": at.config.DialTimeout,
			}),
			wg:          at.wg,
			dialTimeout: at.config.DialTimeout,
		}
		tt.ctx, tt.cancel = context.WithCancel(at.ctx)
		at.tunnels = append(at.tunnels, tt)
	}

	if c.InterfaceName != "" {
		at.monitor = NewNetworkInterfaceMonitor(at.ctx, ll, time.Minute, c.InterfaceName)
	}

	return at, nil
}

type host struct {
	alias    string
	hostport string
	username string
}

func (at *AutoTunnel) Start() error {
	for _, c := range at.connections {
		go c.init()
	}
	for _, t := range at.tunnels {
		go t.start()
	}

	if at.monitor != nil {
		ll := at.ll.WithField("network_interface", at.monitor.Name)
		ll.Info("configuring interface monitor")
		_, err := at.monitor.Start()
		if err != nil {
			ll.WithError(err).Error("interface monitor returned error")
		}
		upChan, downChan := at.monitor.Chans()
		go func() {
			for {
				select {
				case <-at.ctx.Done():
					return
				case <-upChan:
					ll.Info("network interface reported up, unpausing connections")
					for _, c := range at.connections {
						go c.unpause()
					}
				case <-downChan:
					ll.Info("network interface reported down, pausing connections")
					for _, c := range at.connections {
						go c.pause()
					}
				}
			}
		}()
	}

	<-at.ctx.Done()
	for _, t := range at.tunnels {
		t.stop()
	}
	for _, c := range at.connections {
		c.shutdown()
	}

	err := at.ctx.Err()
	at.ll.WithError(err).Info("shutting down")
	at.wg.Wait()
	return err
}

func (t *tunnel) start() {
	t.wg.Add(1)
	t.ll.Info("starting listener")
	listener, err := net.Listen("tcp", "127.0.0.1:"+t.localPort)
	if err != nil {
		t.lastError = err.Error()
		t.ll.WithError(err).Error("starting listener")
		return
	}

	t.listener = listener
	t.accept()
}

func (t *tunnel) stop() {
	t.ll.Info("stopping listener")
	t.cancel()

	if t.listener != nil {
		t.listener.Close()
		t.listener = nil
		t.wg.Done()
	}
}

func (t *tunnel) accept() {
	for t.ctx.Err() == nil {
		conn, err := t.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			t.lastError = err.Error()
			t.ll.WithError(err).Error("accepting connection")
		}

		ll := t.ll.WithField("local_address", conn.RemoteAddr().String())
		ll.Trace("accepted connection")
		go t.forward(ll, conn)
	}
}

func (t *tunnel) trackConnection() {
	atomic.AddUint32(&t.currentConnections, 1)
	t.connection.TrackConnection()
}

func (t *tunnel) trackConnectionClosed() {
	defer atomic.AddUint32(&t.currentConnections, ^uint32(0))
	defer t.connection.TrackConnectionClosed()
}

func (t *tunnel) forward(ll *logrus.Entry, conn net.Conn) {
	var remoteConn net.Conn
	defer func() {
		go conn.Close()
		if remoteConn != nil {
			remoteConn.Close()
		}
	}()

	if t.connection.Paused() {
		ll.Trace("rejecting connection because ssh connection is paused")
		return
	}

	go t.trackConnection()
	defer func() {
		go t.trackConnectionClosed()
	}()

	ll.Trace("forwarding connection")

	errChan := make(chan error)
	go func() {
		<-t.ctx.Done()
		errChan <- t.ctx.Err()
	}()
	go func() {
		ll.Trace("getting ssh client")

		sshClCtx := t.ctx
		if t.dialTimeout != 0 {
			var sshClCancel context.CancelFunc
			sshClCtx, sshClCancel = context.WithTimeout(sshClCtx, t.dialTimeout)
			defer sshClCancel()
		}
		sshClChan := make(chan *ssh.Client)
		go func() {
			sshClChan <- t.connection.client()
		}()

		var (
			cl  *ssh.Client
			err error
		)
		select {
		case cl = <-sshClChan:
		case <-sshClCtx.Done():
			errChan <- fmt.Errorf("getting ssh client: %w", sshClCtx.Err())
			return
		}

		if cl == nil {
			errChan <- fmt.Errorf("failed to get ssh client")
			return
		}

		ll.Trace("dialing remote connection")
		remoteConn, err = cl.Dial("tcp", "0.0.0.0:"+t.remotePort)
		if err != nil {
			ll.WithError(err).Error("dialing remote connection")
			errChan <- fmt.Errorf("dialing remote connection: %w", err)
			return
		}

		ll.Trace("proxying connection")
		go func() {
			_, err = io.Copy(remoteConn, conn)
			if err != nil {
				err = fmt.Errorf("local->remote copy error: %w", err)
			}
			errChan <- err
		}()

		go func() {
			_, err = io.Copy(conn, remoteConn)
			if err != nil {
				err = fmt.Errorf("remote->local copy error: %w", err)
			}
			errChan <- err
		}()
	}()

	err := <-errChan
	if err != nil {
		ll.WithError(err).Error("connection error")
	}
	ll.Trace("connection closed")
}

func (c *connection) start() {
	c.clMutex.Lock()
	defer c.clMutex.Unlock()
	c.wg.Add(1)

	if c.cl != nil {
		// another goroutine has already established a client
		return
	}

	if err := c.ctx.Err(); err != nil {
		c.ll.WithError(err).Error("connection is closed")
		return
	}

	sshConfig := &ssh.ClientConfig{
		User:            c.host.username,
		Auth:            []ssh.AuthMethod{sshAgent()},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         c.dialTimeout,
	}

	var (
		conn net.Conn
		err  error
	)

	ctx := c.ctx
	if c.dialTimeout != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(c.ctx, c.dialTimeout)
		defer cancel()
	}

	ll := c.ll
	if c.proxy != nil {
		conn, err = c.proxy.Dial(ctx, ll, "tcp", c.host.hostport)
	} else {
		ll.Debug("dialing ssh remote")
		d := net.Dialer{Timeout: c.dialTimeout}
		conn, err = d.DialContext(ctx, "tcp", c.host.hostport)
	}
	if err != nil {
		c.lastError = err.Error()
		ll.WithError(err).Error("dialing ssh remote")
		return
	}

	ncc, chans, reqs, err := ssh.NewClientConn(conn, c.host.hostport, sshConfig)
	if err != nil {
		c.lastError = err.Error()
		ll.WithError(err).Error("creating ssh client")
		return
	}

	c.cl = ssh.NewClient(ncc, chans, reqs)
	c.active = true
	go func() {
		err := c.cl.Conn.Wait()
		ll.WithError(err).Debug("ssh client closed")
		c.close()
	}()
}

func (c *connection) init() {
	go func() {
		<-c.ctx.Done()
		err := c.ctx.Err()
		ll := c.ll
		if err != nil {
			ll = ll.WithError(err)
		}
		ll.Info("closing connection")
		c.close()
	}()
}

func (c *connection) client() *ssh.Client {
	c.pausedLock.Lock()
	defer c.pausedLock.Unlock()
	if c.paused {
		return nil
	}

	c.clMutex.RLock()
	if c.cl != nil {
		c.clMutex.RUnlock()
		return c.cl
	}
	c.clMutex.RUnlock()

	c.start()
	c.clMutex.RLock()
	defer c.clMutex.RUnlock()
	return c.cl
}

func (c *connection) shutdown() {
	c.cancel()
}

func (c *connection) close() {
	c.clMutex.Lock()
	defer c.clMutex.Unlock()
	if c.cl == nil {
		c.ll.Trace("already closed")
		return
	}

	c.wg.Done()
	c.active = false
	c.ll.Debug("closing ssh client")
	c.cl.Close()
	c.cl = nil
}

func (c *connection) pause() {
	c.pausedLock.Lock()
	defer c.pausedLock.Unlock()

	if c.paused {
		return
	}

	c.paused = true
	c.close()
}

func (c *connection) unpause() {
	c.pausedLock.Lock()
	defer c.pausedLock.Unlock()
	c.paused = false
}

func (c *connection) TrackConnection() {
	if c.proxy != nil {
		go c.proxy.TrackConnection()
	}
	c.connectionsMutex.Lock()
	defer c.connectionsMutex.Unlock()
	c.currentConnections++

	if c.inactiveTimer != nil {
		c.ll.WithField("inactive_timeout", c.inactiveTimeout).Debug("got connection, interrupting inactive timer")
		c.inactiveTimer.Stop()
		c.inactiveTimer = nil
	}
}

func (c *connection) TrackConnectionClosed() {
	if c.proxy != nil {
		go c.proxy.TrackConnectionClosed()
	}
	c.connectionsMutex.Lock()
	defer c.connectionsMutex.Unlock()
	c.currentConnections--

	if c.currentConnections == 0 && c.inactiveTimeout != 0 {
		c.ll.WithField("inactive_timeout", c.inactiveTimeout).Debug("all active connections were closed, ssh connection will be closed if it remains inactive")
		c.inactiveTimer = time.AfterFunc(c.inactiveTimeout, func() {
			c.connectionsMutex.RLock()
			defer c.connectionsMutex.RUnlock()

			if c.currentConnections == 0 {
				if c.isProxy {
					// give any child connections a little time to shut down cleanly on the other side
					time.Sleep(2 * time.Second)
				}
				c.ll.WithField("inactive_timeout", c.inactiveTimeout).Debug("ssh connection remained inactive, closing")
				c.close()
			}
		})
	}
}

func (c *connection) Paused() bool {
	c.pausedLock.Lock()
	defer c.pausedLock.Unlock()

	return c.paused
}

func sshAgent() ssh.AuthMethod {
	if sshAgent, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK")); err == nil {
		return ssh.PublicKeysCallback(agent.NewClient(sshAgent).Signers)
	}
	return nil
}

type TunnelStatus struct {
	Name              string
	LocalPort         string
	Connected         bool
	ActiveConnections uint32
	LastError         string
	Paused            bool
}

func (at *AutoTunnel) Status() (status []TunnelStatus) {
	for _, t := range at.tunnels {
		var lastErr string
		if t.lastError != "" {
			lastErr = t.lastError
		} else if t.connection.lastError != "" {
			lastErr = t.connection.lastError
		}
		status = append(status, TunnelStatus{
			Name:              t.name,
			LocalPort:         t.localPort,
			LastError:         lastErr,
			Connected:         t.connection.active,
			ActiveConnections: t.currentConnections,
			Paused:            t.connection.paused,
		})
	}
	return
}

type NetworkInterfaceMonitor struct {
	ctx      context.Context
	Name     string
	interval time.Duration
	ll       *logrus.Logger

	lastState        *bool
	upChan, downChan chan struct{}
}

func NewNetworkInterfaceMonitor(ctx context.Context, ll *logrus.Logger, interval time.Duration, name string) *NetworkInterfaceMonitor {
	return &NetworkInterfaceMonitor{
		ctx:      ctx,
		ll:       ll,
		Name:     name,
		interval: interval,
		upChan:   make(chan struct{}, 1),
		downChan: make(chan struct{}, 1),
	}
}

func (c *NetworkInterfaceMonitor) Chans() (chan struct{}, chan struct{}) {
	return c.upChan, c.downChan
}

func (c *NetworkInterfaceMonitor) Start() (bool, error) {
	go func() {
		t := time.NewTicker(c.interval)
		for c.ctx.Err() == nil {
			select {
			case <-t.C:
				_, _ = c.check()
			case <-c.ctx.Done():
				return
			}
		}
	}()

	return c.check()
}

func (c *NetworkInterfaceMonitor) check() (bool, error) {
	i, err := net.InterfaceByName(c.Name)
	if err != nil {
		go c.sendState(false)
		return false, err
	}

	up := i.Flags&net.FlagUp != 0
	if c.lastState == nil {
		c.sendState(up)
		c.lastState = &up
	} else if *c.lastState != up {
		c.sendState(up)
		*c.lastState = up
	}

	return up, nil
}

func (c *NetworkInterfaceMonitor) sendState(up bool) {
	if up {
		c.upChan <- struct{}{}
	} else {
		c.downChan <- struct{}{}
	}
}

func (c *connection) Active() bool {
	return c.active
}

func (c *connection) Dial(ctx context.Context, ll *logrus.Entry, n, addr string) (net.Conn, error) {
	ll = ll.WithField("proxy", c.host.alias)
	ll.Debug("getting proxy client")
	cl := c.client()
	if cl == nil {
		c.lastError = "failed to get proxy ssh client"
		ll.Error("failed to get proxy ssh client")
		return nil, errors.New("failed to get proxy ssh client")
	}

	ll.Debug("dialing ssh remote through proxy")
	return cl.Dial(n, addr)
}

func (c *commandProxy) Active() bool {
	return c.active
}

func (c *commandProxy) Dial(ctx context.Context, ll *logrus.Entry, n, addr string) (net.Conn, error) {
	c.mtx.RLock()
	if c.conn != nil {
		c.mtx.RUnlock()
		return c.conn, nil
	}
	c.mtx.RUnlock()

	c.start()
	c.mtx.RLock()
	defer c.mtx.RUnlock()
	return c.conn, nil
}

func (c *commandProxy) TrackConnection() {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	c.currentConnections++

	if c.inactiveTimer != nil {
		c.ll.WithField("inactive_timeout", c.inactiveTimeout).Debug("got connection, interrupting inactive timer")
		c.inactiveTimer.Stop()
		c.inactiveTimer = nil
	}
}

func (c *commandProxy) TrackConnectionClosed() {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	c.currentConnections--

	if c.currentConnections == 0 && c.inactiveTimeout != 0 {
		c.ll.WithField("inactive_timeout", c.inactiveTimeout).Debug("all active connections were closed, proxy command will be closed if it remains inactive")
		c.inactiveTimer = time.AfterFunc(c.inactiveTimeout, func() {
			c.mtx.RLock()
			defer c.mtx.RUnlock()

			if c.currentConnections == 0 {
				c.ll.WithField("inactive_timeout", c.inactiveTimeout).Debug("proxy command remained inactive, closing")
				c.close()
			}
		})
	}
}

func (c *commandProxy) start() {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	if c.process != nil && c.process.Pid > 0 {
		// another goroutine has already established a client
		return
	}

	if err := c.ctx.Err(); err != nil {
		c.ll.WithError(err).Error("context is closed")
		return
	}

	p1, p2 := net.Pipe()
	var (
		name string
		args []string
	)
	if len(c.command) > 0 {
		name = c.command[0]
	}
	if len(c.command) > 1 {
		args = c.command[1:]
	}
	cmd := exec.CommandContext(c.ctx, name, args...)
	cmd.Stdin = p1
	cmd.Stdout = p1
	cmd.Stderr = os.Stderr
	err := cmd.Start()
	if err != nil {
		c.ll.WithError(err).Warn("can't start proxy command")
	}
	c.wg.Add(1)
	ll := c.ll.WithField("pid", cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		ll.WithError(err).Debug("proxy command exited")
		c.close()
		c.process = nil
		c.conn = nil
		c.wg.Done()
	}()
	c.conn = p2
}

func (c *commandProxy) close() {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.process == nil {
		c.ll.Trace("no running proxy process")
		return
	}

	c.active = false
	c.ll.Debug("terminating proxy process")
	_ = c.process.Signal(syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	if c.process.Pid > 0 {
		c.ll.Debug("proxy process took longer than 100ms to exit; killing")
		_ = c.process.Kill()
	}
}
