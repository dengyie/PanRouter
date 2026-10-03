package service

import "testing"

// 表驱动覆盖设计文档 §4.3 决策表的全部关键组合。
func TestRouteDecision(t *testing.T) {
	chromeUA := "Mozilla/5.0 (Windows NT 10.0) Chrome/126.0.0.0 Safari/537.36"
	cases := []struct {
		name string
		l    LinkMeta
		env  RouteEnv
		want string
	}{
		{"裸链home", LinkMeta{}, RouteEnv{Profile: "home", ClientUA: chromeUA}, "302"},
		{"裸链cloud仍可302", LinkMeta{}, RouteEnv{Profile: "cloud", ClientUA: chromeUA}, "302"},
		{"带Cookie禁302", LinkMeta{Cookie: "__puus=1"}, RouteEnv{Profile: "home", ClientUA: chromeUA}, "aria2"},
		{"要求Referer禁302", LinkMeta{Referer: "https://pan.quark.cn/"}, RouteEnv{Profile: "home", ClientUA: chromeUA}, "aria2"},
		{"UA一致可302", LinkMeta{UA: chromeUA}, RouteEnv{Profile: "home", ClientUA: chromeUA}, "302"},
		{"UA不一致走aria2", LinkMeta{UA: "quark-cloud-drive UA"}, RouteEnv{Profile: "home", ClientUA: chromeUA}, "aria2"},
		{"链要求UA但客户端UA未知", LinkMeta{UA: chromeUA}, RouteEnv{Profile: "home"}, "aria2"},
		{"绑IP同机aria2", LinkMeta{BindIP: true}, RouteEnv{Profile: "home", ClientUA: chromeUA, Aria2SameHost: true}, "aria2"},
		{"绑IP异机走stream", LinkMeta{BindIP: true}, RouteEnv{Profile: "cloud", ClientUA: chromeUA, Aria2SameHost: false}, "stream"},
		{"绑IP加Cookie异机走stream", LinkMeta{BindIP: true, Cookie: "k=v"}, RouteEnv{Profile: "cloud", Aria2SameHost: false}, "stream"},
	}
	for _, c := range cases {
		if got := Route(c.l, c.env); got != c.want {
			t.Errorf("%s: Route()=%s want %s", c.name, got, c.want)
		}
	}
}

func TestCan302Rules(t *testing.T) {
	env := RouteEnv{Profile: "home", ClientUA: "UA-X"}
	if Can302(LinkMeta{}, env) != true {
		t.Error("裸链应可 302")
	}
	if Can302(LinkMeta{BindIP: true}, env) != false {
		t.Error("绑 IP 一律禁 302(保守处理,见设计文档 §4.3)")
	}
	if Can302(LinkMeta{Cookie: "k=v"}, env) != false {
		t.Error("带 Cookie 校验禁 302")
	}
	if Can302(LinkMeta{UA: "ua-x"}, env) != true {
		t.Error("UA 大小写不敏感匹配应可 302")
	}
}
