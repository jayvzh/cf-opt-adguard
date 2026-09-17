package ipselector

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestSelectCFSTRealFixture(t *testing.T) {
	ips, err := Select(context.Background(), Options{
		Source:     "cfst:testdata/result.csv",
		IPVersions: []int{4},
	}, nil)
	if err != nil {
		t.Fatalf("解析真实夹具失败: %v", err)
	}
	if len(ips) != 244 {
		t.Fatalf("IP 数 %d want 244", len(ips))
	}
	if got := First(ips).String(); got != "104.21.88.54" {
		t.Fatalf("最优 IP %s want 104.21.88.54", got)
	}
	for _, ip := range ips {
		if ip.To4() == nil {
			t.Fatalf("混入非 v4: %s", ip)
		}
	}
}

func TestSelectCFSTHeaderOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.csv")
	writeFile(t, path, "IP 地址,已发送,已接收,丢包率,平均延迟,下载速度(MB/s),地区码\n")
	_, err := Select(context.Background(), Options{Source: "cfst:" + path}, nil)
	if !errors.Is(err, ErrNoIP) {
		t.Fatalf("err=%v want ErrNoIP", err)
	}
}

func TestSelectCFSTMissingFile(t *testing.T) {
	_, err := Select(context.Background(), Options{Source: "cfst:no_such.csv"}, nil)
	if !errors.Is(err, ErrNoIP) {
		t.Fatalf("err=%v want ErrNoIP", err)
	}
}

func TestSelectVersionFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ips.txt")
	writeFile(t, path, "104.21.88.54\n2606:4700:4700::1111\n")

	// [4] 过滤掉 v6。
	ips, err := Select(context.Background(), Options{Source: "static:" + path, IPVersions: []int{4}}, nil)
	if err != nil || len(ips) != 1 || ips[0].To4() == nil {
		t.Fatalf("v4 过滤: ips=%v err=%v", ips, err)
	}
	// [6] 只留 v6。
	ips, err = Select(context.Background(), Options{Source: "static:" + path, IPVersions: []int{6}}, nil)
	if err != nil || len(ips) != 1 || ips[0].To4() != nil {
		t.Fatalf("v6 过滤: ips=%v err=%v", ips, err)
	}
	// [4,6] 全留。
	ips, err = Select(context.Background(), Options{Source: "static:" + path, IPVersions: []int{4, 6}}, nil)
	if err != nil || len(ips) != 2 {
		t.Fatalf("双族: ips=%v err=%v", ips, err)
	}
	// 配置版本与结果无交集 → ErrNoIP。
	_, err = Select(context.Background(), Options{Source: "cfst:testdata/result.csv", IPVersions: []int{6}}, nil)
	if !errors.Is(err, ErrNoIP) {
		t.Fatalf("全过滤后 err=%v want ErrNoIP", err)
	}
}

func TestSelectStaticCommentsAndInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ips.txt")
	writeFile(t, path, "# 注释\n\n104.21.88.54\nnot-an-ip\n  104.25.250.92  \n")
	ips, err := Select(context.Background(), Options{Source: "static:" + path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 || ips[0].String() != "104.21.88.54" || ips[1].String() != "104.25.250.92" {
		t.Fatalf("ips=%v", ips)
	}
}

func TestSelectInvalidSource(t *testing.T) {
	for _, src := range []string{"", "bogus", "http://x", "unknown:abc"} {
		if _, err := Select(context.Background(), Options{Source: src}, nil); err == nil {
			t.Errorf("source=%q 应报错", src)
		}
	}
}

// startFakeDNS 起本地 UDP 假 DNS 返回预置 A 记录。
func startFakeDNS(t *testing.T, answers map[string][]dns.RR) string {
	t.Helper()
	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if rrs, ok := answers[r.Question[0].Name]; ok {
			m.Answer = rrs
		} else {
			m.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(m)
	})
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: pc, Handler: mux}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	return pc.LocalAddr().String()
}

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(s)
	if err != nil {
		t.Fatal(err)
	}
	return rr
}

// slowListener accept 前 sleep，模拟高延迟节点；返回监听地址。
func slowListener(t *testing.T, d time.Duration) *net.TCPAddr {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			time.Sleep(d)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr)
}

func TestSelectDomainStrategies(t *testing.T) {
	addr := startFakeDNS(t, map[string][]dns.RR{
		"cfip.example.com.": {
			mustRR(t, "cfip.example.com. 300 IN A 1.2.3.4"),
			mustRR(t, "cfip.example.com. 300 IN A 5.6.7.8"),
		},
	})

	// roundrobin：保持解析顺序。
	ips, err := Select(context.Background(), Options{
		Source: "domain:cfip.example.com", Strategy: "roundrobin",
		Resolvers: []string{addr}, Timeout: time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 || ips[0].String() != "1.2.3.4" || ips[1].String() != "5.6.7.8" {
		t.Fatalf("roundrobin 顺序: %v", ips)
	}

	// lowest_latency：1.2.3.4 / 5.6.7.8 不可达 → 全失败保序（探测失败沉底逻辑）。
	ips, err = Select(context.Background(), Options{
		Source: "domain:cfip.example.com", Strategy: "lowest_latency",
		Resolvers: []string{addr}, Timeout: 200 * time.Millisecond,
	}, nil)
	if err != nil || len(ips) != 2 || ips[0].String() != "1.2.3.4" {
		t.Fatalf("lowest_latency 全失败保序: ips=%v err=%v", ips, err)
	}
}

func TestSortByTCP443(t *testing.T) {
	// 全部不可达 → 保持原序（探测失败沉底）。
	unreachable := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2")}
	sortByTCP443(unreachable, "1", 200*time.Millisecond) // 端口 1 通常无服务
	if unreachable[0].String() != "127.0.0.1" || unreachable[1].String() != "127.0.0.2" {
		t.Fatalf("全失败应保序: %v", unreachable)
	}

	// 拨通者优先：可达 listener 排到不可达 IP 之前。
	fastAddr := slowListener(t, 0)
	reachable := []net.IP{net.ParseIP("203.0.113.9"), fastAddr.IP}
	sortByTCP443(reachable, strconv.Itoa(fastAddr.Port), time.Second)
	if reachable[0].String() != fastAddr.IP.String() {
		t.Fatalf("拨通者应排前: %v", reachable)
	}
}

func TestFirstEmpty(t *testing.T) {
	if First(nil) != nil {
		t.Fatal("空列表 First 应为 nil")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
