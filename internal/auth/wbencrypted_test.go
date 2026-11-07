package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/wbencrypted-auth.json 是**合成**向量：用 wbencrypted.go 里记录的
// at-rest 密钥 + 文档化的 AAD 拼接口径加密而成（生成脚本走的是 DistPub/workbuddy2api
// 的同一套参数），不是任何真实账号的凭据。三个字段刻意覆盖三种明文形态：
// 纯文本 token、Buffer 包装的昵称、数字型的 expiresAt。
const wbVectorPath = "testdata/wbencrypted-auth.json"

func TestParseWBEncryptedAuthFile(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析加密 auth 文件失败: %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"accessToken", a.AccessToken, "at-from-wbencrypted"},
		{"refreshToken", a.RefreshToken, "rt-from-wbencrypted"},
		{"nickname", a.Nickname, "小凛"},
		{"uid", a.UID, "uid-wb-1"},
		{"deviceToken", a.DeviceToken, "v3:test-device-token"},
		{"realm", a.Realm(), "cn"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q want %q", c.name, c.got, c.want)
		}
	}
	// expiresAt 在文件里是毫秒，Parse 后必须归一成秒。
	if a.ExpiresAt != 1800000000 {
		t.Errorf("expiresAt = %d want 1800000000（毫秒应归一为秒）", a.ExpiresAt)
	}
}

// AAD 是最容易错一个字节的地方：这里对着参考实现的字节串逐位校验。
func TestWBFieldAADMatchesReference(t *testing.T) {
	const want = "57422d41414400010000000557424556310000000673796d2d763100000001" +
		"0000001039313237646561316234343032306137020000"
	got := hex.EncodeToString(wbFieldAAD(1, wbKeyID(sha256.Sum256([]byte(wbAtRestSecretKey)))))
	if got != want {
		t.Fatalf("AAD 与参考实现不一致:\n got %s\nwant %s", got, want)
	}
}

// 桌面端换过 at-rest 密钥时必须报错而不是解出乱码当 token 用。
func TestParseWBEncryptedKeyIDMismatchFails(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("向量不是合法 JSON: %v", err)
	}
	authObj := doc["auth"].(map[string]any)
	env := authObj["accessToken"].(map[string]any)
	// 换掉 envelope 里的 keyId（解 base64 再改再编回）。
	rawEnv, err := base64.StdEncoding.DecodeString(env["envelope"].(string))
	if err != nil {
		t.Fatalf("信封不是合法 base64: %v", err)
	}
	keyID := wbKeyID(sha256.Sum256([]byte(wbAtRestSecretKey)))
	plainEnv := strings.Replace(string(rawEnv), keyID, "0000000000000000", 1)
	if plainEnv == string(rawEnv) {
		t.Fatal("向量里找不到 keyId，测试自身失效")
	}
	env["envelope"] = base64.StdEncoding.EncodeToString([]byte(plainEnv))
	bad, _ := json.Marshal(doc)

	if _, err := Parse(bad); err == nil {
		t.Fatal("keyId 不匹配时应当报错")
	} else if !strings.Contains(err.Error(), "at_rest_key_mismatch") {
		t.Fatalf("错误应说明是密钥不匹配，实际: %v", err)
	}
}

// 刷新后的写回：来源是加密文件，就只能回写加密形态——明文覆盖桌面端文件是把
// 别人的登录态写成我们能读、桌面端读不了的样子。
func TestSaveAtomicKeepsEnvelopeForEncryptedSource(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), "auth.info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写临时 auth 失败: %v", err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	a.FilePath = path
	a.AccessToken = "at-refreshed"
	a.RefreshToken = "rt-refreshed"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if strings.Contains(string(saved), "at-refreshed") || strings.Contains(string(saved), "rt-refreshed") {
		t.Fatal("写回文件里出现了明文 token")
	}
	if !strings.Contains(string(saved), `"$wbEncrypted"`) {
		t.Fatal("写回文件丢了信封形态")
	}
	again, err := Parse(saved)
	if err != nil {
		t.Fatalf("回写后再解析失败: %v", err)
	}
	if again.AccessToken != "at-refreshed" || again.RefreshToken != "rt-refreshed" {
		t.Fatalf("回写后的 token 读不回来: %+v", again)
	}
	if again.Nickname != "小凛" {
		t.Fatalf("回写后的昵称读不回来: %q", again.Nickname)
	}
}

// 客户端不加密的字段，回写也必须留明文。
//
// 实测客户端的信封清单是 accessToken / refreshToken / nickname / phoneNumber，
// expiresAt 明文。按固定清单去封会把它读作明文的 expiresAt 加密，桌面端就读不回来。
func TestSaveAtomicLeavesPlainFieldsPlain(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("向量不是合法 JSON: %v", err)
	}
	doc["auth"].(map[string]any)["expiresAt"] = float64(1800000000000) // 还原成明文数字
	raw, _ = json.Marshal(doc)

	path := filepath.Join(t.TempDir(), "auth.info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写临时 auth 失败: %v", err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	saved, _ := os.ReadFile(path)
	var back map[string]any
	if err := json.Unmarshal(saved, &back); err != nil {
		t.Fatalf("写回文件不是合法 JSON: %v", err)
	}
	if _, isEnvelope := back["auth"].(map[string]any)["expiresAt"].(map[string]any); isEnvelope {
		t.Error("expiresAt 原本明文，回写不该加密")
	}
	if _, isEnvelope := back["auth"].(map[string]any)["accessToken"].(map[string]any); !isEnvelope {
		t.Error("accessToken 原本加密，回写必须仍是信封")
	}
	if again, err := Parse(saved); err != nil || again.ExpiresAt != 1800000000 {
		t.Errorf("回写后再解析失败或到期时间不对: %v %+v", err, again)
	}
}

// 未加密文件必须原样走老路径（零回归）。
func TestParseUnencryptedStillWorks(t *testing.T) {
	a, err := Parse([]byte(`{"auth":{"accessToken":"at","refreshToken":"rt","realm":"global"},"account":{"uid":"u1"}}`))
	if err != nil {
		t.Fatalf("明文 auth 解析失败: %v", err)
	}
	if a.AccessToken != "at" || a.UID != "u1" || a.Realm() != "global" {
		t.Fatalf("明文解析结果不对: %+v", a)
	}
}
