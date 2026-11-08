//go:build identitytest

package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

const identityTestPprofEnv = "BT_IDENTITYTEST_PPROF_ADDRESS"

var profilingListener atomic.Pointer[net.Listener]

// startProfiling serves net/http/pprof on its own listener when
// BT_IDENTITYTEST_PPROF_ADDRESS is set (identitytest builds only; the release
// binary links no pprof), never on the API handler. The address must not be
// public (checkProfilingAddress). Mutex and block profiling are sampled at
// rates low enough for the 30-minute soak: 1 in 100 contention events, and
// one block sample per 10 ms blocked on average (every block of 10 ms or
// more, shorter ones in proportion to their duration).
func startProfiling(logger *slog.Logger) func() {
	address := os.Getenv(identityTestPprofEnv)
	if address == "" {
		return func() {}
	}
	if err := checkProfilingAddress(address, net.InterfaceAddrs); err != nil {
		logger.Error("pprof address refused", slog.String("address", address), slog.String("error", err.Error()))
		return func() {}
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		logger.Error("pprof listener", slog.String("error", err.Error()))
		return func() {}
	}
	profilingListener.Store(&listener)
	runtime.SetMutexProfileFraction(100)
	runtime.SetBlockProfileRate(int((10 * time.Millisecond).Nanoseconds()))
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	logger.Info("pprof listening", slog.String("address", listener.Addr().String()))
	return func() {
		_ = server.Close()
		profilingListener.CompareAndSwap(&listener, nil)
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	}
}

// profilingAddress is the bound listener's address ("" when none), for tests.
func profilingAddress() string {
	if l := profilingListener.Load(); l != nil {
		return (*l).Addr().String()
	}
	return ""
}

// checkProfilingAddress refuses a listener other machines could reach. An
// explicit host must be loopback (an IP, or localhost); a private, ULA or
// link-local host is refused (a LAN or tailnet address). A wildcard host
// (":6060", "0.0.0.0", "::") listens on every interface, so it is accepted
// only when every interface address is loopback, private or link-local: it
// exists for a container on its bridge networks, published to the host's
// loopback by Compose (Task 1). A native run must use 127.0.0.1:<port>: on a
// host whose only addresses are private, the wildcard would pass and listen
// on the LAN. The check runs once, at startup; an interface that comes up
// later (a VPN, an IPv6 address) is not re-checked. Any other host name is
// refused (its resolution cannot be vouched for).
func checkProfilingAddress(address string, interfaceAddrs func() ([]net.Addr, error)) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("not a host:port address")
	}
	if host == "localhost" {
		return nil
	}
	if host != "" {
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return errors.New("a host name other than localhost")
		}
		if !ip.IsUnspecified() {
			if !ip.Unmap().IsLoopback() {
				return errors.New("an explicit address that is not loopback")
			}
			return nil
		}
	}
	addresses, err := interfaceAddrs()
	if err != nil {
		return errors.New("a wildcard address with unreadable interfaces")
	}
	for _, address := range addresses {
		var ip net.IP
		switch value := address.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		parsed, ok := netip.AddrFromSlice(ip)
		if !ok || !nonPublic(parsed) {
			return errors.New("a wildcard address on a host with a public interface address")
		}
	}
	return nil
}

func nonPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
