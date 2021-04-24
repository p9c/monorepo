package state

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/go-socks/socks"
	"github.com/p9c/qu"
	"go.uber.org/atomic"

	"github.com/p9c/log"
	"github.com/p9c/matrjoska/cmd/node/active"
	"github.com/p9c/matrjoska/pkg/amt"
	"github.com/p9c/matrjoska/pkg/apputil"
	"github.com/p9c/matrjoska/pkg/chaincfg"
	"github.com/p9c/matrjoska/pkg/chainrpc"
	"github.com/p9c/matrjoska/pkg/connmgr"
	"github.com/p9c/matrjoska/pkg/fork"
	"github.com/p9c/matrjoska/pkg/pipe"
	"github.com/p9c/matrjoska/pkg/util"
	"github.com/p9c/matrjoska/pkg/util/routeable"
	"github.com/p9c/matrjoska/pod/config"
)

// GetNew returns a fresh new context
func GetNew(
	config *config.Config, hf func(ifc interface{}) error,
	quit qu.C,
) (s *State, e error) {
	// after this, all the configurations are set and mostly sanitized
	if e = config.Initialize(hf); E.Chk(e) {
		// return
		panic(e)
	}
	log.SetLogLevel(config.LogLevel.V())
	chainClientReady := qu.T()
	rand.Seed(time.Now().UnixNano())
	rand.Seed(rand.Int63())
	s = &State{
		ChainClientReady: chainClientReady,
		KillAll:          quit,
		Config:           config,
		ConfigMap:        config.Map,
		StateCfg:         new(active.Config),
		NodeChan:         make(chan *chainrpc.Server),
		Syncing:          atomic.NewBool(true),
	}
	// everything in the configuration is set correctly up to this point, except for settings based on the running
	// network, so after this is when those settings are elaborated
	T.Ln("setting active network:", s.Config.Network.V())
	switch s.Config.Network.V() {
	case "testnet", "testnet3", "t":
		s.ActiveNet = &chaincfg.TestNet3Params
		fork.IsTestnet = true
		// fork.HashReps = 3
	case "regtestnet", "regressiontest", "r":
		fork.IsTestnet = true
		s.ActiveNet = &chaincfg.RegressionTestParams
	case "simnet", "s":
		fork.IsTestnet = true
		s.ActiveNet = &chaincfg.SimNetParams
	default:
		if s.Config.Network.V() != "mainnet" &&
			s.Config.Network.V() != "m" {
			D.Ln("using mainnet for node")
		}
		s.ActiveNet = &chaincfg.MainNetParams
	}
	if (s.Config.LAN.True() || s.Config.Solo.True()) && s.ActiveNet.Name == "mainnet" {
		if e = fmt.Errorf("neither Solo or LAN can be active on mainnet for obvious reasons"); F.Chk(e) {
			return
		}
	}
	// if pipe logging is enabled, start it up
	if s.Config.PipeLog.True() {
		D.Ln("starting up pipe logger")
		pipe.LogServe(s.KillAll, fmt.Sprint(os.Args))
	}
	// set to write logs in the network specific directory, if the value was not set and is not the same as datadir
	if s.Config.LogDir.V() == s.Config.DataDir.V() {
		e = s.Config.LogDir.Set(filepath.Join(s.Config.DataDir.V(), s.ActiveNet.Name))
	}
	// set up TLS stuff if it hasn't been set up yet. We assume if the configured values correspond to files the files
	// are valid TLS cert/pairs, and that the key will be absent if onetimetlskey was set
	if (s.Config.ClientTLS.True() || s.Config.ServerTLS.True()) &&
		(
			(!apputil.FileExists(s.Config.RPCKey.V()) && s.Config.OneTimeTLSKey.False()) ||
				!apputil.FileExists(s.Config.RPCCert.V()) ||
				!apputil.FileExists(s.Config.CAFile.V())) {
		D.Ln("generating TLS certificates")
		I.Ln(s.Config.RPCKey.V(), s.Config.RPCCert.V(), s.Config.RPCKey.V())
		// Create directories for cert and key files if they do not yet exist.
		certDir, _ := filepath.Split(s.Config.RPCCert.V())
		keyDir, _ := filepath.Split(s.Config.RPCKey.V())
		e = os.MkdirAll(certDir, 0700)
		if e != nil {
			E.Ln(e)
			return
		}
		e = os.MkdirAll(keyDir, 0700)
		if e != nil {
			E.Ln(e)
			return
		}
		// Generate cert pair.
		org := "pod/wallet autogenerated cert"
		validUntil := time.Now().Add(time.Hour * 24 * 365 * 10)
		var cert, key []byte
		cert, key, e = util.NewTLSCertPair(org, validUntil, nil)
		if e != nil {
			E.Ln(e)
			return
		}
		_, e = tls.X509KeyPair(cert, key)
		if e != nil {
			E.Ln(e)
			return
		}
		// Write cert and (potentially) the key files.
		e = ioutil.WriteFile(s.Config.RPCCert.V(), cert, 0600)
		if e != nil {
			rmErr := os.Remove(s.Config.RPCCert.V())
			if rmErr != nil {
				E.Ln("cannot remove written certificates:", rmErr)
			}
			return
		}
		e = ioutil.WriteFile(s.Config.CAFile.V(), cert, 0600)
		if e != nil {
			rmErr := os.Remove(s.Config.RPCCert.V())
			if rmErr != nil {
				E.Ln("cannot remove written certificates:", rmErr)
			}
			return
		}
		e = ioutil.WriteFile(s.Config.RPCKey.V(), key, 0600)
		if e != nil {
			E.Ln(e)
			rmErr := os.Remove(s.Config.RPCCert.V())
			if rmErr != nil {
				E.Ln("cannot remove written certificates:", rmErr)
			}
			rmErr = os.Remove(s.Config.CAFile.V())
			if rmErr != nil {
				E.Ln("cannot remove written certificates:", rmErr)
			}
			return
		}
		D.Ln("done generating TLS certificates")
	}

	// Validate profile port number
	T.Ln("validating profile port number")
	if s.Config.Profile.V() != "" {
		var profilePort int
		profilePort, e = strconv.Atoi(s.Config.Profile.V())
		if e != nil || profilePort < 1024 || profilePort > 65535 {
			e = fmt.Errorf("the profile port must be between 1024 and 65535, disabling profiling")
			E.Ln(e)
			return
			// if e = s.Config.Profile.Set(""); E.Chk(e) {
			// }
		}
	}

	T.Ln("checking addpeer and connectpeer lists")
	if s.Config.AddPeers.Len() > 0 && s.Config.ConnectPeers.Len() > 0 {
		e = fmt.Errorf("the addpeers and connectpeers options can not be both set")
		_, _ = fmt.Fprintln(os.Stderr, e)
		return
	}

	T.Ln("checking proxy/connect for disabling listening")
	if (s.Config.ProxyAddress.V() != "" || s.Config.ConnectPeers.Len() > 0) && s.Config.P2PListeners.Len() == 0 {
		s.Config.DisableListen.T()
	}

	T.Ln("checking relay/reject nonstandard policy settings")
	switch {
	case s.Config.RelayNonStd.True() && s.Config.RejectNonStd.True():
		e = fmt.Errorf("rejectnonstd and relaynonstd cannot be used together" +
			" -- choose only one")
		E.Ln(e)
		return
	}

	// Chk to make sure limited and admin users don't have the same username
	T.Ln("checking admin and limited username is different")
	if !s.Config.Username.Empty() &&
		s.Config.Username.V() == s.Config.LimitUser.V() {
		e := fmt.Errorf("--username and --limituser must not specify the same username")
		_, _ = fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	// Chk to make sure limited and admin users don't have the same password
	T.Ln("checking limited and admin passwords are not the same")
	if !s.Config.Password.Empty() &&
		s.Config.Password.V() == s.Config.LimitPass.V() {
		e := fmt.Errorf("password and limitpass must not specify the same password")
		_, _ = fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}

	T.Ln("checking user agent comments", s.Config.UserAgentComments)
	for _, uaComment := range s.Config.UserAgentComments.S() {
		if strings.ContainsAny(uaComment, "/:()") {
			e = fmt.Errorf(
				"the following characters must not " +
					"appear in user agent comments: '/', ':', '(', ')'",
			)
			_, _ = fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
	}

	T.Ln("checking min relay tx fee")
	s.StateCfg.ActiveMinRelayTxFee, e = amt.NewAmount(s.Config.MinRelayTxFee.V())
	if e != nil {
		E.Ln(e)
		str := "invalid minrelaytxfee: %v"
		e = fmt.Errorf(str, e)
		_, _ = fmt.Fprintln(os.Stderr, e)
		os.Exit(0)
	}
	I.Ln("autolisten", s.Config.AutoListen.True())
	// if autolisten is set, set default ports on all p2p listeners discovered to be available
	if s.Config.AutoListen.True() {
		I.Ln("autolisten is enabled")
		_, allAddresses := routeable.GetAddressesAndInterfaces()
		p2pAddresses := []string{}
		for addr := range allAddresses {
			p2pAddresses = append(p2pAddresses, net.JoinHostPort(addr, s.ActiveNet.DefaultPort))
		}
		if e = s.Config.P2PConnect.Set(p2pAddresses); E.Chk(e) {
			return
		}
		if e = s.Config.P2PListeners.Set(p2pAddresses); E.Chk(e) {
			return
		}
		s.StateCfg.Save = true
	}

	// if autoports is set, in addition, set all listeners to random automatic ports, this and autolisten can be
	// combined for full auto, this enables multiple instances on one local host
	var fP int
	if s.Config.AutoPorts.True() {
		I.Ln("autoports is enabled")
		p2pl := [][]string{
			s.Config.P2PListeners.V(), s.Config.RPCListeners.V(),
			s.Config.WalletRPCListeners.V(),
		}
		for i := range p2pl {
			for j := range p2pl[i] {
				var h string
				if h, _, e = net.SplitHostPort(p2pl[i][j]); E.Chk(e) {
					return
				}
				if fP, e = GetFreePort(); E.Chk(e) {
					return
				}
				addy := net.JoinHostPort(h, fmt.Sprint(fP))
				p2pl[i][j] = addy
			}
		}
		if e = s.Config.P2PListeners.Set(p2pl[0]); E.Chk(e) {
			return
		}
		if e = s.Config.RPCListeners.Set(p2pl[1]); E.Chk(e) {
			return
		}
		if e = s.Config.WalletRPCListeners.Set(p2pl[2]); E.Chk(e) {
			return
		}

		s.StateCfg.Save = true
	}
	// if LAN or Solo are active, disable DNS seeding
	if s.Config.LAN.True() || s.Config.Solo.True() {
		W.Ln("disabling DNS seeding due to test mode settings active")
		s.Config.DisableDNSSeed.T()
	}

	T.Ln("checking rpc server has a login enabled")
	if (s.Config.Username.Empty() || s.Config.Password.Empty()) &&
		(s.Config.LimitUser.Empty() || s.Config.LimitPass.Empty()) {
		W.Ln("disabling RPC due to empty login credentials")
		s.Config.DisableRPC.T()
	}
	T.Ln("checking rpc max concurrent requests")
	if s.Config.RPCMaxConcurrentReqs.V() < 0 {
		str := "The rpcmaxwebsocketconcurrentrequests opt may not be" +
			" less than 0 -- parsed [%d]"
		e = fmt.Errorf(str, s.Config.RPCMaxConcurrentReqs.V())
		_, _ = fmt.Fprintln(os.Stderr, e)
		// os.Exit(1)
		return
	}
	// Setup dial and DNS resolution (lookup) functions depending on the specified
	// options. The default is to use the standard net.DialTimeout function as well
	// as the system DNS resolver. When a proxy is specified, the dial function is
	// set to the proxy specific dial function and the lookup is set to use tor
	// (unless --noonion is specified in which case the system DNS resolver is
	// used).
	T.Ln("setting network dialer and lookup")
	s.StateCfg.Dial = net.DialTimeout
	s.StateCfg.Lookup = net.LookupIP
	if !s.Config.ProxyAddress.Empty() {
		T.Ln("we are loading a proxy!")
		_, _, e = net.SplitHostPort(s.Config.ProxyAddress.V())
		if e != nil {
			E.Ln(e)
			str := "proxy address '%s' is invalid: %v"
			e = fmt.Errorf(str, s.Config.ProxyAddress.V(), e)
			fmt.Fprintln(os.Stderr, e)
			// os.Exit(1)
			return
		}
		// TODO: this is kinda stupid hm? switch *and* toggle by presence of flag value, one should be enough
		if s.Config.OnionEnabled.True() && !s.Config.OnionProxyAddress.Empty() {
			E.Ln("onion enabled but no onionproxy has been configured")
			T.Ln("halting to avoid exposing IP address")
		}
		// Tor stream isolation requires either proxy or onion proxy to be set.
		if s.Config.TorIsolation.True() &&
			s.Config.ProxyAddress.Empty() &&
			s.Config.OnionProxyAddress.Empty() {
			e = fmt.Errorf("%s: Tor stream isolation requires either proxy or onionproxy to be set")
			_, _ = fmt.Fprintln(os.Stderr, e)
			return
			// os.Exit(1)
		}
		if s.Config.OnionEnabled.False() {
			if e = s.Config.OnionProxyAddress.Set(""); E.Chk(e) {
			}
		}

		// Tor isolation flag means proxy credentials will be overridden unless there is
		// also an onion proxy configured in which case that one will be overridden.
		torIsolation := false
		if s.Config.TorIsolation.True() && s.Config.OnionProxyAddress.Empty() &&
			(!s.Config.ProxyUser.Empty() || !s.Config.ProxyPass.Empty()) {
			torIsolation = true
			W.Ln(
				"Tor isolation set -- overriding specified" +
					" proxy user credentials",
			)
		}
		proxy := &socks.Proxy{
			Addr:         s.Config.ProxyAddress.V(),
			Username:     s.Config.ProxyUser.V(),
			Password:     s.Config.ProxyPass.V(),
			TorIsolation: torIsolation,
		}
		s.StateCfg.Dial = proxy.DialTimeout
		// Treat the proxy as tor and perform DNS resolution through it unless the
		// --noonion flag is set or there is an onion-specific proxy configured.
		if s.Config.OnionEnabled.True() &&
			s.Config.OnionProxyAddress.Empty() {
			s.StateCfg.Lookup = func(host string) ([]net.IP, error) {
				return connmgr.TorLookupIP(host, s.Config.ProxyAddress.V())
			}
		}
	}
	// Setup onion address dial function depending on the specified options. The
	// default is to use the same dial function selected above. However, when an
	// onion-specific proxy is specified, the onion address dial function is set to
	// use the onion-specific proxy while leaving the normal dial function as
	// selected above. This allows .onion address traffic to be routed through a
	// different proxy than normal traffic.
	T.Ln("setting up tor proxy if enabled")
	if !s.Config.OnionProxyAddress.Empty() {
		if _, _, e = net.SplitHostPort(s.Config.OnionProxyAddress.V()); E.Chk(e) {
			e = fmt.Errorf("onion proxy address '%s' is invalid: %v",
				s.Config.OnionProxyAddress.V(), e,
			)
			// _, _ = fmt.Fprintln(os.Stderr, e)
		}
		// Tor isolation flag means onion proxy credentials will be overridden.
		if s.Config.TorIsolation.True() &&
			(!s.Config.OnionProxyUser.Empty() || !s.Config.OnionProxyPass.Empty()) {
			W.Ln(
				"Tor isolation set - overriding specified onionproxy user" +
					" credentials",
			)
		}
	}
	T.Ln("setting onion dialer")
	s.StateCfg.Oniondial =
		func(network, addr string, timeout time.Duration) (net.Conn, error) {
			proxy := &socks.Proxy{
				Addr:         s.Config.OnionProxyAddress.V(),
				Username:     s.Config.OnionProxyUser.V(),
				Password:     s.Config.OnionProxyPass.V(),
				TorIsolation: s.Config.TorIsolation.True(),
			}
			return proxy.DialTimeout(network, addr, timeout)
		}

	// When configured in bridge mode (both --onion and --proxy are configured), it
	// means that the proxy configured by --proxy is not a tor proxy, so override
	// the DNS resolution to use the onion-specific proxy.
	T.Ln("setting proxy lookup")
	if !s.Config.ProxyAddress.Empty() {
		s.StateCfg.Lookup = func(host string) ([]net.IP, error) {
			return connmgr.TorLookupIP(host, s.Config.OnionProxyAddress.V())
		}
	} else {
		s.StateCfg.Oniondial = s.StateCfg.Dial
	}
	// Specifying --noonion means the onion address dial function results in an error.
	if s.Config.OnionEnabled.False() {
		s.StateCfg.Oniondial = func(a, b string, t time.Duration) (
			net.Conn, error,
		) {
			return nil, errors.New("tor has been disabled")
		}
	}
	if s.StateCfg.Save || !apputil.FileExists(s.Config.ConfigFile.V()) {
		s.StateCfg.Save = false
		if s.Config.RunningCommand.Name == "kopach" {
			return
		}
		D.Ln("saving configuration")
		var e error
		if e = s.Config.WriteToFile(s.Config.ConfigFile.V()); E.Chk(e) {
		}
	}
	if s.ActiveNet.Name == chaincfg.TestNet3Params.Name {
		fork.IsTestnet = true
	}

	return
}

// GetFreePort asks the kernel for free open ports that are ready to use.
func GetFreePort() (int, error) {
	var port int
	addr, e := net.ResolveTCPAddr("tcp", "localhost:0")
	if e != nil {
		return 0, e
	}
	var l *net.TCPListener
	l, e = net.ListenTCP("tcp", addr)
	if e != nil {
		return 0, e
	}
	defer func() {
		if e := l.Close(); E.Chk(e) {
		}
	}()
	port = l.Addr().(*net.TCPAddr).Port
	return port, nil
}
