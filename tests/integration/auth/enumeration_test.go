package authit

import (
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

// AC4/AC7：不存在账号与口令错误（及 realm 不符、账号停用）的响应完全相同——状态码、响应头、响应体逐项一致，
// 并且耗时分布无显著差异（不存在账号也做一次完整的 argon2id 计算）。
func TestNoAccountEnumeration(t *testing.T) {
	s := newStack(t, stackOpts{settings: map[string]string{"login.throttle_account_max": "100000", "login.throttle_ip_max": "100000"}})
	real := s.seedUser("platform")
	disabled := s.seedUser("platform", withStatus("disabled"))
	tenantUser := s.seedUser("tenant")
	ip := freshIP()

	kinds := map[string]func(i int) resp{
		"口令错误": func(int) resp { return failLogin(s, "admin", real.Email, ip) },
		"用户不存在": func(i int) resp {
			return failLogin(s, "admin", "nobody-"+strings.ToLower(ids.MustNew())+"@example.test", ip)
		},
		"realm 不符（口令正确）": func(int) resp {
			return s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: tenantUser.Email, Password: tenantUser.Password}, withIP(ip))
		},
		"账号停用（口令正确）": func(int) resp {
			return s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: disabled.Email, Password: disabled.Password}, withIP(ip))
		},
	}
	// 1) 逐项一致。
	var refName string
	var ref resp
	for name, f := range kinds {
		r := f(0)
		s.contract("login", "POST", "/auth/login", r)
		if refName == "" {
			refName, ref = name, r
			continue
		}
		if r.Status != ref.Status || stripReqID(r.Body) != stripReqID(ref.Body) {
			t.Errorf("%q 与 %q 的响应不一致:\n%d %s\n%d %s", name, refName, r.Status, r.Body, ref.Status, ref.Body)
		}
		if h1, h2 := stableHeaders(r), stableHeaders(ref); h1 != h2 {
			t.Errorf("%q 与 %q 的响应头不一致:\n%s\n%s", name, refName, h1, h2)
		}
	}
	if ref.Status != 401 {
		t.Fatalf("期望 401: %d", ref.Status)
	}

	// 2) 耗时分布：交错采样，避免时间漂移偏向某一类。
	const n = 40
	samples := map[string][]float64{}
	order := []string{"口令错误", "用户不存在", "realm 不符（口令正确）", "账号停用（口令正确）"}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < n; i++ {
		perm := append([]string(nil), order...)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] }) // 随机化每轮顺序，消除位置效应
		for _, name := range perm {
			r := kinds[name](i)
			samples[name] = append(samples[name], float64(r.Dur)/float64(time.Millisecond))
		}
	}
	// 对照：入参校验失败（400）在口令哈希之前返回，耗时应明显更短——证明这个统计检查有区分力。
	for i := 0; i < n; i++ {
		r := s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: "bad", Password: shortPW}, withIP(ip))
		samples["对照:400"] = append(samples["对照:400"], float64(r.Dur)/float64(time.Millisecond))
	}
	base := samples["口令错误"]
	for _, name := range order[1:] {
		z := mannWhitneyZ(base, samples[name])
		m1, m2 := median(base), median(samples[name])
		t.Logf("%-18s 中位数 %.1fms（口令错误 %.1fms） Mann-Whitney z=%.2f", name, m2, m1, z)
		if math.Abs(z) > 4.5 || math.Abs(m1-m2)/math.Max(m1, m2) > 0.30 {
			t.Errorf("%q 与「口令错误」的耗时分布差异显著（中位数 %.1fms vs %.1fms，z=%.2f）", name, m2, m1, z)
		}
	}
	ctl := samples["对照:400"]
	if median(ctl) > 0.5*median(base) {
		t.Errorf("对照组（不做哈希）应明显更快，统计检查才有区分力：%.1fms vs %.1fms", median(ctl), median(base))
	}
	if z := mannWhitneyZ(base, ctl); math.Abs(z) < 6 {
		t.Errorf("对照组与哈希路径的 z=%.2f，应显著不同（检查灵敏度不足）", z)
	}
}

// stableHeaders 返回会随请求变化之外的响应头（排除 X-Request-ID、Date）。
func stableHeaders(r resp) string {
	var ks []string
	for k, v := range r.Header {
		if k == "X-Request-Id" || k == "Date" || k == "Content-Length" {
			continue
		}
		ks = append(ks, k+"="+strings.Join(v, "|"))
	}
	sort.Strings(ks)
	return strings.Join(ks, "\n")
}

func median(x []float64) float64 {
	c := append([]float64(nil), x...)
	sort.Float64s(c)
	return c[len(c)/2]
}

// mannWhitneyZ 返回 Mann-Whitney U 检验的正态近似 z 值（含并列修正前的简化版本；样本量 ≥ 20 时足够）。
func mannWhitneyZ(a, b []float64) float64 {
	type obs struct {
		v   float64
		grp int
	}
	all := make([]obs, 0, len(a)+len(b))
	for _, v := range a {
		all = append(all, obs{v, 0})
	}
	for _, v := range b {
		all = append(all, obs{v, 1})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v < all[j].v })
	ranks := make([]float64, len(all))
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].v == all[i].v {
			j++
		}
		r := float64(i+j+1) / 2 // 并列取平均秩
		for k := i; k < j; k++ {
			ranks[k] = r
		}
		i = j
	}
	var ra float64
	for i, o := range all {
		if o.grp == 0 {
			ra += ranks[i]
		}
	}
	n1, n2 := float64(len(a)), float64(len(b))
	u := ra - n1*(n1+1)/2
	mu := n1 * n2 / 2
	sd := math.Sqrt(n1 * n2 * (n1 + n2 + 1) / 12)
	return (u - mu) / sd
}
