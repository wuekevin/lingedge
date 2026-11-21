package baota

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseVhostTable(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantErr   bool
		sites     int
		names     []string // 第一个 server 的 server_name
		listens   []Listen
		ssl       bool
		malformed bool
	}{
		{"基本", "server { listen 80; server_name a.com b.com; }", false, 1, []string{"a.com", "b.com"}, []Listen{{Addr: "*", Port: 80}}, false, false},
		{"注释与行尾注释", "# 开头\nserver { # 块注释\n listen 80; # 端口\n #listen 81;\n server_name a.com; #b.com\n}", false, 1, []string{"a.com"}, []Listen{{Addr: "*", Port: 80}}, false, false},
		{"多 listen + ssl + default_server", "server { listen 80 default_server; listen 443 ssl http2; listen [::]:443 ssl; listen 127.0.0.1:8080; server_name x; }", false, 1, []string{"x"},
			[]Listen{{Addr: "*", Port: 80, Default: true}, {Addr: "*", Port: 443, SSL: true}, {Addr: "::", Port: 443, SSL: true}, {Addr: "127.0.0.1", Port: 8080}}, true, false},
		{"IPv6 回环无端口、地址无端口", "server { listen [::1]; listen 10.0.0.1; server_name x; }", false, 1, []string{"x"}, []Listen{{Addr: "::1", Port: 80}, {Addr: "10.0.0.1", Port: 80}}, false, false},
		{"unix 套接字", "server { listen unix:/run/x.sock; server_name x; }", false, 1, []string{"x"}, []Listen{{Unix: "/run/x.sock"}}, false, false},
		{"quic/udp", "server { listen 443 quic reuseport; server_name x; }", false, 1, []string{"x"}, []Listen{{Addr: "*", Port: 443, UDP: true}}, false, false},
		{"引号与转义", `server { server_name "a b.com" 'c.com' d\.com; listen 80; }`, false, 1, []string{"a b.com", "c.com", `d\.com`}, []Listen{{Addr: "*", Port: 80}}, false, false},
		{"ssl_certificate 视为 ssl", "server { listen 80; server_name x; ssl_certificate /a; }", false, 1, []string{"x"}, []Listen{{Addr: "*", Port: 80}}, true, false},
		{"location 内的 listen 不算", "server { listen 80; server_name x; location / { listen 9999; server_name inner; } }", false, 1, []string{"x"}, []Listen{{Addr: "*", Port: 80}}, false, false},
		{"嵌套 http", "http { include x; server { listen 80; server_name a; } server { listen 81; server_name b; } }", false, 2, []string{"a"}, []Listen{{Addr: "*", Port: 80}}, false, false},
		{"空文件", "", false, 0, nil, nil, false, false},
		{"只有注释", "# nothing\n", false, 0, nil, nil, false, false},
		{"缺少右括号", "server { listen 80; server_name a;", true, 1, []string{"a"}, []Listen{{Addr: "*", Port: 80}}, false, true},
		{"多余右括号", "server { listen 80; } }", true, 1, nil, []Listen{{Addr: "*", Port: 80}}, false, true},
		{"引号未闭合", "server { listen 80; server_name \"a.com; }", true, 1, nil, nil, false, true},
		{"缺少分号", "server { listen 80; server_name a.com }", true, 0, nil, nil, false, false},
		{"没有名字的块", "{ listen 80; }", true, 0, nil, nil, false, false},
		{"非法端口被忽略", "server { listen 99999; listen abc; listen 80; server_name x; }", false, 1, []string{"x"}, []Listen{{Addr: "*", Port: 80}}, false, false},
		{"二进制垃圾", "\x00\xff\xfe{{{;;;}}}\x01", true, 0, nil, nil, false, false},
	}
	for _, c := range cases {
		sites, err := ParseVhost("/x.conf", "system", []byte(c.in))
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v，期望 error=%v", c.name, err, c.wantErr)
		}
		if c.name == "缺少分号" {
			// 缺 ; 的指令仍被保留（尽力解析），server 块由于最后一个 `}` 配对仍成立，但整体标记畸形
			if len(sites) != 1 || !sites[0].Malformed || !reflect.DeepEqual(sites[0].ServerNames, []string{"a.com"}) {
				t.Errorf("%s: sites=%+v", c.name, sites)
			}
			continue
		}
		if c.name == "没有名字的块" || c.name == "二进制垃圾" {
			continue
		}
		if len(sites) != c.sites {
			t.Errorf("%s: sites=%d，期望 %d", c.name, len(sites), c.sites)
			continue
		}
		if c.sites == 0 {
			continue
		}
		s := sites[0]
		if c.names != nil && !reflect.DeepEqual(s.ServerNames, c.names) {
			t.Errorf("%s: server_name=%q，期望 %q", c.name, s.ServerNames, c.names)
		}
		if c.listens != nil && !reflect.DeepEqual(s.Listens, c.listens) {
			t.Errorf("%s: listens=%+v，期望 %+v", c.name, s.Listens, c.listens)
		}
		if s.SSL != c.ssl {
			t.Errorf("%s: ssl=%v", c.name, s.SSL)
		}
		if s.Malformed != c.malformed {
			t.Errorf("%s: malformed=%v", c.name, s.Malformed)
		}
	}
}

// 病态输入：极深嵌套、海量 token、超长行——不 panic、不死循环、有上限。
func TestParseVhostPathological(t *testing.T) {
	deep := strings.Repeat("a {", 50000) + "server { listen 80; }" + strings.Repeat("}", 50000)
	if _, err := ParseVhost("/x.conf", "system", []byte(deep)); err == nil {
		t.Error("嵌套过深应报告 error")
	}
	many := strings.Repeat("server { listen 80; server_name a; }\n", 5000)
	sites, _ := ParseVhost("/x.conf", "system", []byte(many))
	if len(sites) != maxServers {
		t.Errorf("server 数应被限制在 %d，得到 %d", maxServers, len(sites))
	}
	names := "server { server_name " + strings.Repeat("n.example.com ", 5000) + "; }"
	sites, _ = ParseVhost("/x.conf", "system", []byte(names))
	if len(sites) != 1 || len(sites[0].ServerNames) != maxNamesPerSrv {
		t.Errorf("server_name 数应被限制在 %d", maxNamesPerSrv)
	}
	big := make([]byte, maxConfBytes+1000)
	for i := range big {
		big[i] = 'x'
	}
	if _, err := ParseVhost("/x.conf", "system", big); err == nil {
		t.Error("超大文件应报告截断")
	}
}

func FuzzParseVhost(f *testing.F) {
	for _, s := range []string{
		"server { listen 80; server_name a.com; }",
		"server {\n listen [::]:443 ssl http2 default_server;\n server_name \"a b\" 'c' ~^x$;\n # c\n location / { return 200; }\n}",
		"{{{ ;;; }}} \"unterminated",
		"http { server { listen unix:/x; } }",
		"server { listen 99999999999999999999; server_name \\",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		sites, _ := ParseVhost("/f.conf", "system", b)
		for _, s := range sites {
			if len(s.ServerNames) > maxNamesPerSrv {
				t.Fatal("server_name 超限")
			}
			for _, l := range s.Listens {
				if l.Port < 0 || l.Port > 65535 {
					t.Fatalf("端口越界: %+v", l)
				}
			}
		}
	})
}
