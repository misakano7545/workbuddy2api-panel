package panel

import (
	"strings"
	"testing"
)

// TestAccountCheckinRealmEcho 签到响应里的 realm 必须是账号自己的 realm。
//
// 曾是字面量 "cn"：当时靠上方 `a.IsGlobal()` 提前返回兜着，值恰好永远对。但门控一挪
// （放宽成「国际号也走签到」、或提前返回被重构掉）国际号就会拿到 "cn"，而这类错误在
// 编译、池层测试里都看不见——只在响应体里。
func TestAccountCheckinRealmEcho(t *testing.T) {
	e := upgNewEnv(t,
		upgSeed{uid: "cn-acct", nickname: "cn", credits: 10},
		upgSeed{uid: "gl-acct", nickname: "gl", realm: "global", credits: 10},
	)
	for uid, want := range map[string]string{"cn-acct": "cn", "gl-acct": "global"} {
		code, body := upgPost(t, e.panel, "/panel/api/accounts/"+uid+"/checkin")
		if code != 200 {
			t.Fatalf("%s 签到应 200, got %d (%s)", uid, code, body)
		}
		if !strings.Contains(body, `"realm":"`+want+`"`) {
			t.Fatalf("%s 回显 realm 应为 %q, got %s", uid, want, body)
		}
	}
}
