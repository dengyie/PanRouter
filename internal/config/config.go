// Package config 负责配置加载、默认值与热加载。
package config

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
)

type Server struct {
	Listen        string        `mapstructure:"listen"`
	BaseURL       string        `mapstructure:"base_url"` // 对外访问地址,用于生成 /d、/stream 链接
	DeployProfile string        `mapstructure:"deploy_profile"`
	SignTTL       time.Duration `mapstructure:"sign_ttl"`
	DataDir       string        `mapstructure:"data_dir"`
}

type Auth struct {
	Username       string `mapstructure:"username"`
	PasswordBcrypt string `mapstructure:"password_bcrypt"`
	APIToken       string `mapstructure:"api_token"`
	JWTSecret      string `mapstructure:"jwt_secret"`
}

type DriverCommon struct {
	Enabled       bool     `mapstructure:"enabled"`
	UpstreamAllow []string `mapstructure:"upstream_allow"`
	RedirectAllow []string `mapstructure:"redirect_allow"`
	Proxy         string   `mapstructure:"proxy"`
	LimitQPS      float64  `mapstructure:"limit_qps"`
	DownloadConc  int      `mapstructure:"download_concurrency"`
}

type Aria2 struct {
	Endpoint string `mapstructure:"endpoint"`
	Secret   string `mapstructure:"secret"`
	SameHost bool   `mapstructure:"same_host"`
}

type Log struct {
	Level string `mapstructure:"level"`
}

type Config struct {
	Server       Server                  `mapstructure:"server"`
	Auth         Auth                    `mapstructure:"auth"`
	DomainRoutes map[string][]string     `mapstructure:"domain_routes"` // 分享链接域名 → 网盘 ID
	Drivers      map[string]DriverCommon `mapstructure:"drivers"`       // 以网盘 ID 为键;新增网盘 = 加一个键 + main 工厂注册
	Aria2        Aria2                   `mapstructure:"aria2"`
	Log          Log                     `mapstructure:"log"`
}

// 默认凭据常量:非 loopback 监听时若仍是默认值,main 会拒绝启动(安全守卫)。
const (
	DefaultAPIToken  = "panrouter-dev-token"
	DefaultJWTSecret = "panrouter-dev-jwt-secret-change-me"
)

// Default 返回一份带缺省值的配置(M1 预设 home 画像,见设计文档 §1.2)。
func Default() *Config {
	c := &Config{}
	c.Server.Listen = "127.0.0.1:6400"
	c.Server.BaseURL = "http://127.0.0.1:6400"
	c.Server.DeployProfile = "home"
	c.Server.SignTTL = 72 * time.Hour
	c.Server.DataDir = "./data"
	c.Auth.Username = "admin"
	// "admin123" 的 bcrypt 哈希,上线前必须更换
	c.Auth.PasswordBcrypt = "$2a$10$ZmboqbL3NvvVCXSDUR3ev.EWMUKRFo6OjQObNE3K6b.XggHNyuT9K"
	c.Auth.APIToken = DefaultAPIToken
	c.Auth.JWTSecret = DefaultJWTSecret
	c.DomainRoutes = map[string][]string{
		"quark":  {"pan.quark.cn"},
		"lanzou": {"lanzou.com", "lanzouw.com", "lanzoui.com", "lanzoue.com", "lanzouf.com", "lanzoa.com", "lanzoub.com", "lanzouc.com", "lanzoud.com", "lanzouv.com", "lanzoux.com", "lansov.com", "lanzn.com", "lanzouq.com", "lanzouy.com", "lanzouu.com"},
	}
	c.Drivers = map[string]DriverCommon{
		"quark": {
			Enabled: true, LimitQPS: 2, DownloadConc: 3,
			UpstreamAllow: []string{".quark.cn"},
			RedirectAllow: []string{".quark.cn", ".quarkcdn.cn", ".uc.cn"},
		},
		"lanzou": {
			Enabled: true, LimitQPS: 5, DownloadConc: 3,
			UpstreamAllow: []string{"lanzou.com", "lanzouw.com", "lanzoui.com", "lanzoue.com", "lanzouf.com", "lanzoa.com", "lanzoub.com", "lanzouc.com", "lanzoud.com", "lanzouv.com", "lanzoux.com", "lansov.com", "lanzn.com", "lanzouq.com", "lanzouy.com", "lanzouu.com", ".xlig.cn", ".feijipan.com", ".lanrar.com", ".dmpdmp.com"},
			RedirectAllow: []string{"lanzou.com", "lanzouw.com", "lanzoui.com", "lanzoue.com", "lanzouf.com", "lanzoa.com", "lanzoub.com", "lanzouc.com", "lanzoud.com", "lanzouv.com", "lanzoux.com", "lansov.com", "lanzn.com", "lanzouq.com", "lanzouy.com", "lanzouu.com", ".xlig.cn", ".lanrar.com", ".dmpdmp.com"},
		},
	}
	c.Aria2.Endpoint = "http://127.0.0.1:6800/jsonrpc"
	c.Aria2.SameHost = true
	c.Log.Level = "info"
	return c
}

// Provider 提供并发安全的配置读取与热加载。
type Provider struct {
	p atomic.Pointer[Config]
}

func (p *Provider) Set(c *Config) { p.p.Store(c) }
func (p *Provider) Get() *Config  { return p.p.Load() }

// Load 从 path 读取 YAML 并合并默认值;path 为空时依次尝试 ./config.yaml、./config/config.yaml。
func Load(path string) (*Config, string, error) {
	v := viper.New()
	c := Default()
	v.SetConfigType("yaml")
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("config")
		v.AddConfigPath(".")
		v.AddConfigPath("./config")
	}
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok && path == "" {
			// 显式路径未找到才允许默认值;文件存在但解析失败必须报错,
			// 否则会带着默认密钥静默启动(安全回归)。
			return c, "", nil
		}
		return nil, "", err
	}
	if err := v.Unmarshal(c); err != nil {
		return nil, "", err
	}
	c.Server.DeployProfile = strings.ToLower(c.Server.DeployProfile)
	if c.Server.DeployProfile != "cloud" {
		c.Server.DeployProfile = "home"
	}
	return c, v.ConfigFileUsed(), nil
}
