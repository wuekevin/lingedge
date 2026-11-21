package baota

// eachString 逐个访问 Report 中所有字符串（用于泄漏检查）。
func (r Report) eachString(f func(field, v string)) {
	c := r
	c.mapStringsNamed(func(name, v string) string { f(name, v); return v })
}

// mapStrings 对每个字符串应用 f（原地修改接收者的切片元素，调用方应传入副本——Scrub 先复制了切片）。
func (r *Report) mapStrings(f func(string) string) {
	r.mapStringsNamed(func(_, v string) string { return f(v) })
}

func (r *Report) mapStringsNamed(f func(name, v string) string) {
	r.ProbedAt = f("probed_at", r.ProbedAt)
	r.Confidence = f("confidence", r.Confidence)
	r.Version = f("version", r.Version)
	r.VersionSource = f("version_source", r.VersionSource)
	r.Evidence = append([]Evidence(nil), r.Evidence...)
	for i := range r.Evidence {
		e := &r.Evidence[i]
		e.Kind, e.Path, e.Detail = f("evidence.kind", e.Kind), f("evidence.path", e.Path), f("evidence.detail", e.Detail)
	}
	r.WebServers = append([]WebServer(nil), r.WebServers...)
	for i := range r.WebServers {
		w := &r.WebServers[i]
		w.Variant, w.Kind, w.Binary, w.Conf, w.VhostDir, w.Version = f("web.variant", w.Variant), f("web.kind", w.Kind), f("web.binary", w.Binary), f("web.conf", w.Conf), f("web.vhost_dir", w.VhostDir), f("web.version", w.Version)
	}
	r.Databases = append([]DBInstall(nil), r.Databases...)
	for i := range r.Databases {
		d := &r.Databases[i]
		d.Variant, d.Kind, d.Path, d.DataDir, d.Version = f("db.variant", d.Variant), f("db.kind", d.Kind), f("db.path", d.Path), f("db.data_dir", d.DataDir), f("db.version", d.Version)
	}
	r.PHP = append([]PHP(nil), r.PHP...)
	for i := range r.PHP {
		r.PHP[i].Version, r.PHP[i].Path = f("php.version", r.PHP[i].Version), f("php.path", r.PHP[i].Path)
	}
	r.Sites = append([]Site(nil), r.Sites...)
	for i := range r.Sites {
		s := &r.Sites[i]
		s.File, s.Source = f("site.file", s.File), f("site.source", s.Source)
		s.ServerNames = append([]string(nil), s.ServerNames...)
		for j := range s.ServerNames {
			s.ServerNames[j] = f("site.server_name", s.ServerNames[j])
		}
		s.Listens = append([]Listen(nil), s.Listens...)
		for j := range s.Listens {
			s.Listens[j].Addr, s.Listens[j].Unix = f("site.listen.addr", s.Listens[j].Addr), f("site.listen.unix", s.Listens[j].Unix)
		}
	}
	r.Firewalls = append([]Firewall(nil), r.Firewalls...)
	for i := range r.Firewalls {
		x := &r.Firewalls[i]
		x.Kind, x.Active, x.Source = f("fw.kind", x.Kind), f("fw.active", x.Active), f("fw.source", x.Source)
	}
	r.WAF = append([]WAF(nil), r.WAF...)
	for i := range r.WAF {
		r.WAF[i].Name, r.WAF[i].Path = f("waf.name", r.WAF[i].Name), f("waf.path", r.WAF[i].Path)
	}
	r.Plugins = append([]string(nil), r.Plugins...)
	for i := range r.Plugins {
		r.Plugins[i] = f("plugin", r.Plugins[i])
	}
	r.Services = append([]Service(nil), r.Services...)
	for i := range r.Services {
		s := &r.Services[i]
		s.Name, s.Unit, s.Active, s.Role = f("svc.name", s.Name), f("svc.unit", s.Unit), f("svc.active", s.Active), f("svc.role", s.Role)
	}
	r.Notes = append([]string(nil), r.Notes...)
	for i := range r.Notes {
		r.Notes[i] = f("note", r.Notes[i])
	}
}
