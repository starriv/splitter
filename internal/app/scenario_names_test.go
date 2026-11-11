package app

// scenarioNames lists real-world names resolved by client devices, grouped
// by category.
var scenarioNames = []string{
	// Apple devices
	"icloud.com", "p30-caldav.icloud.com", "apple.com", "www.apple.com.cn", "mzstatic.com", "is1-ssl.mzstatic.com",
	"gs.apple.com", "captive.apple.com", "time.apple.com", "push.apple.com", "1-courier.push.apple.com", "mask.icloud.com",
	"swscan.apple.com", "configuration.apple.com", "itunes.apple.com", "app-site-association.cdn-apple.com",
	// Windows / Microsoft
	"windowsupdate.com", "download.windowsupdate.com", "msftconnecttest.com", "www.msftconnecttest.com",
	"login.microsoftonline.com", "outlook.office365.com", "teams.microsoft.com", "officecdn.microsoft.com",
	// Xiaomi router / phones / IoT
	"api.io.mi.com", "account.xiaomi.com", "miui.com", "update.miui.com", "xiaomi.net", "mi.com", "ot.io.mi.com",
	// Common Chinese apps
	"weixin.qq.com", "wx.qq.com", "mp.weixin.qq.com", "qq.com", "www.qq.com", "y.qq.com", "gtimg.cn", "qpic.cn",
	"alipay.com", "mobilegw.alipay.com", "alipayobjects.com", "taobao.com", "www.taobao.com", "tmall.com", "alicdn.com",
	"img.alicdn.com", "aliyun.com", "ntp.aliyun.com", "baidu.com", "www.baidu.com", "bdstatic.com",
	"bilibili.com", "www.bilibili.com", "api.bilibili.com", "hdslb.com", "i0.hdslb.com", "bilivideo.com",
	"163.com", "music.163.com", "music.126.net", "jd.com", "360buyimg.com", "meituan.com", "dianping.com",
	"douyin.com", "snssdk.com", "zhihu.com", "zhimg.com", "xiaohongshu.com", "xhscdn.com", "ctrip.com", "12306.cn",
	"gov.cn", "www.gov.cn", "edu.cn", "goproxy.cn", "mirrors.aliyun.com", "mirrors.tuna.tsinghua.edu.cn",
	// Overseas services of Chinese companies (must not reach the CN upstream: @!cn)
	"tiktok.com", "www.tiktok.com", "capcut.com", "lemon8-app.com", "wechat.com",
	// Developer / infra
	"github.com", "api.github.com", "raw.githubusercontent.com", "objects.githubusercontent.com", "ghcr.io",
	"registry-1.docker.io", "production.cloudflare.docker.com", "registry.npmjs.org", "proxy.golang.org", "sum.golang.org",
	"pypi.org", "files.pythonhosted.org", "crates.io", "static.crates.io", "deb.debian.org", "archive.ubuntu.com",
	"controlplane.tailscale.com", "derp1.tailscale.com", "login.tailscale.com", "api.telegram.org", "anthropic.com",
	"claude.ai", "api.anthropic.com", "openai.com", "chatgpt.com", "cloudflare.com", "one.one.one.one",
	"letsencrypt.org", "acme-v02.api.letsencrypt.org", "example.net", "www.example.net",
	// Media and general foreign services
	"google.com", "www.google.com", "gstatic.com", "googleapis.com", "youtube.com", "i.ytimg.com", "googlevideo.com",
	"twitter.com", "x.com", "twimg.com", "facebook.com", "instagram.com", "netflix.com", "nflxvideo.net", "spotify.com",
	"wikipedia.org", "reddit.com", "steampowered.com", "store.steampowered.com", "steamcommunity.com", "steamserver.net",
	"epicgames.com", "playstation.net", "nintendo.net",
	// Local-zone names and unusual names forwarded by a front-end resolver
	"local.example", "home.local.example", "dns.local.example", "a.b.c.local.example",
	"nas", "wpad", "localhost", "router.lan", "printer.local", "_dns.resolver.arpa", "home.arpa",
	"1.31.168.192.in-addr.arpa", "x.example", "cn", "com.cn", "xn--fiqs8s",
}
