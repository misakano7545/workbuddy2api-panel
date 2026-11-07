// wbencrypted.go 解密新版桌面端 auth 文件里的 $wbEncrypted 字段。
//
// 逆向来源：DistPub/workbuddy2api 的 wb_auth_decrypt.js + converter.py
// （对 WorkBuddy.app 逆向得到）。新版桌面端把 auth 文件里 accessToken /
// refreshToken / expiresAt / nickname 等字段换成信封加密：
//
//	{"$wbEncrypted":1,"envelope":"<base64(JSON{suite,keyId,nonce,authTag,ciphertext})>"}
//
// key = sha256(utf8(atRestSecretKey 字符串本身))，AES-256-GCM；
// keyId = sha256(key) 的 hex 前 16 位，用于校验信封是否由同一把密钥产生；
// AAD 是固定二进制拼接（见 wbFieldAAD）。密钥来自 Electron 原生绑定
// electron_browser_workbuddy_storage 的 loggerGet()，随版本可能变——keyId
// 对不上时报错，不猜。无 $wbEncrypted 字段的文件原样返回，零成本。
package auth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// wbAtRestSecretKey 桌面端 at-rest 静态密钥（canonical base64，44 位）。
// 桌面端升级后若 keyId 不再匹配，需重新从 loggerGet() 取，改这里。
const wbAtRestSecretKey = "Sik9U5aXhCdwTVEwsEySDOmDoB9r9ntFxHF1fst9LQI="

// decryptWBEncrypted 递归解出 doc 里所有 $wbEncrypted 字段并重新 marshal。
// 无加密字段时返回原字节（不做二次序列化，避免把客户端文件格式改写）；
// sealed 记录哪些字段原本是信封（形如 "auth.accessToken"）——回写时按这份清单
// 复原，见 sealForSave。
func decryptWBEncrypted(raw []byte) (out []byte, sealed map[string]bool, err error) {
	if !bytes.Contains(raw, []byte(`"$wbEncrypted"`)) {
		return raw, nil, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 交给调用方按原路径报 storage_parse_error，这里不抢错误。
		return raw, nil, nil
	}
	key := sha256.Sum256([]byte(wbAtRestSecretKey))
	sealed = map[string]bool{}
	v, changed, err := wbDecryptValue(doc, key, "", sealed)
	if err != nil || !changed {
		return raw, nil, err
	}
	out, err = json.Marshal(v)
	if err != nil {
		return nil, nil, fmt.Errorf("at_rest_marshal_failed: %w", err)
	}
	return out, sealed, nil
}

// wbDecryptValue 深度遍历；命中信封就地替换。changed=false 表示整棵树没有加密字段。
// prefix 是当前节点的路径（"auth.accessToken"），命中信封时记进 sealed。
func wbDecryptValue(v any, key [32]byte, prefix string, sealed map[string]bool) (any, bool, error) {
	switch t := v.(type) {
	case map[string]any:
		if enc, ok := t["$wbEncrypted"].(float64); ok && enc == 1 {
			if env, ok := t["envelope"].(string); ok && env != "" {
				plain, err := wbDecryptEnvelope(env, key)
				if err != nil {
					return nil, false, err
				}
				sealed[prefix] = true
				return plain, true, nil
			}
		}
		changed := false
		for k, item := range t {
			nv, c, err := wbDecryptValue(item, key, wbJoinPath(prefix, k), sealed)
			if err != nil {
				return nil, false, err
			}
			if c {
				t[k] = nv
				changed = true
			}
		}
		return t, changed, nil
	case []any:
		changed := false
		for i, item := range t {
			nv, c, err := wbDecryptValue(item, key, fmt.Sprintf("%s[%d]", prefix, i), sealed)
			if err != nil {
				return nil, false, err
			}
			if c {
				t[i] = nv
				changed = true
			}
		}
		return t, changed, nil
	}
	return v, false, nil
}

// wbJoinPath 拼 sealed 路径；顶层容器不加前缀（"auth.accessToken"）。
func wbJoinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// sealForSave 只重新封装「读入时确实是信封」的字段。
//
// 为什么不按固定清单封：客户端封哪些字段是它自己的契约（实测封 accessToken /
// refreshToken / nickname / phoneNumber，expiresAt 保持明文），照我们的清单去封会把
// 它读作明文的字段也加密，桌面端就读不回来了。来源文件没加密的字段一律不碰。
// 任一字段封装失败即报错（调用方不得落盘），不静默降级成明文。
func sealForSave(doc map[string]any, sealed map[string]bool) error {
	for path := range sealed {
		parts := strings.Split(path, ".")
		if len(parts) != 2 {
			continue // accounts[]/allAccounts[] 这类我们不回写的容器，跳过
		}
		parent, ok := doc[parts[0]].(map[string]any)
		if !ok {
			continue
		}
		v, ok := parent[parts[1]]
		if !ok || v == nil {
			continue
		}
		if _, isEnvelope := v.(map[string]any); isEnvelope {
			continue // 已经是信封
		}
		env, err := sealWBValue(v)
		if err != nil {
			return err
		}
		parent[parts[1]] = env
	}
	// 凭据两个字段必须封上：漏了就是明文 token 覆盖加密文件。
	for _, k := range []string{"accessToken", "refreshToken"} {
		if authObj, ok := doc["auth"].(map[string]any); ok && sealed["auth."+k] {
			if _, isEnvelope := authObj[k].(map[string]any); !isEnvelope {
				return fmt.Errorf("at_rest_seal_failed: %s 未能封装，拒绝以明文落盘", k)
			}
		}
	}
	return nil
}

// wbDecryptEnvelope 解一个 base64 信封，返回明文解包后的值。
func wbDecryptEnvelope(envelopeB64 string, key [32]byte) (any, error) {
	rawEnv, err := base64.StdEncoding.DecodeString(envelopeB64)
	if err != nil {
		return nil, fmt.Errorf("at_rest_bad_envelope: %w", err)
	}
	var env struct {
		Suite      uint32 `json:"suite"`
		KeyID      string `json:"keyId"`
		Nonce      string `json:"nonce"`
		AuthTag    string `json:"authTag"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(rawEnv, &env); err != nil {
		return nil, fmt.Errorf("at_rest_bad_envelope: %w", err)
	}
	wantID := wbKeyID(key)
	if env.KeyID != wantID {
		return nil, fmt.Errorf("at_rest_key_mismatch: envelope keyId=%s want %s"+
			"（桌面端换了 at-rest 密钥，需重新逆向 loggerGet）", env.KeyID, wantID)
	}
	nonce, tag, ct, err := b64Triple(env.Nonce, env.AuthTag, env.Ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("at_rest_cipher_failed: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("at_rest_gcm_failed: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, append(ct, tag...), wbFieldAAD(env.Suite, env.KeyID))
	if err != nil {
		return nil, fmt.Errorf("at_rest_decrypt_failed: %w", err)
	}
	return wbDecodePlain(plain), nil
}

// wbKeyID sha256(sha256(secret)) 的 hex 前 16 位——信封里的 keyId 校验口径。
func wbKeyID(key [32]byte) string {
	h := sha256.Sum256(key[:])
	return hex.EncodeToString(h[:])[:16]
}

// wbFieldAAD 字段级 AAD（framing=2）。任意一个字节不同都会解密失败。
func wbFieldAAD(suite uint32, keyID string) []byte {
	var b bytes.Buffer
	b.WriteString("WB-AAD\x00")
	b.WriteByte(1)
	wbWriteLP(&b, "WBEV1")  // framing tag
	wbWriteLP(&b, "sym-v1") // scheme
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], suite)
	b.Write(u[:])
	wbWriteLP(&b, keyID)
	b.WriteByte(2) // framing: file=1 field=2 record=3 stream=4
	b.WriteByte(0) // 无 sequence
	b.WriteByte(0) // 无 final
	return b.Bytes()
}

func wbWriteLP(b *bytes.Buffer, s string) {
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], uint32(len(s)))
	b.Write(u[:])
	b.WriteString(s)
}

func b64Triple(nonce, tag, ct string) (n, t, c []byte, err error) {
	for _, f := range []struct {
		name string
		in   string
		out  *[]byte
	}{{"nonce", nonce, &n}, {"authTag", tag, &t}, {"ciphertext", ct, &c}} {
		v, e := base64.StdEncoding.DecodeString(f.in)
		if e != nil {
			return nil, nil, nil, fmt.Errorf("at_rest_bad_envelope: %s: %w", f.name, e)
		}
		*f.out = v
	}
	return n, t, c, nil
}

// wbDecodePlain 明文可能是：纯文本、JSON、或是 Buffer 形态的 JSON
// （{"type":"Buffer","data":[字节]}——UTF-8 字节包装），个别字段还会双重 JSON。
func wbDecodePlain(plain []byte) any {
	var v any
	if json.Unmarshal(plain, &v) != nil {
		return string(plain)
	}
	v = wbUnwrapBuffer(v)
	if s, ok := v.(string); ok {
		var again any
		if json.Unmarshal([]byte(s), &again) == nil {
			return wbUnwrapBuffer(again)
		}
	}
	return v
}

// sealWBValue 把一个值包成 $wbEncrypted 信封，明文取值的文本形态
// （字符串原样，其余走 JSON）。SaveAtomic 回写时用它，避免把用完就刷新的
// 凭据以明文覆盖掉桌面端的加密文件。
//
// shortcut: 只按 sym-v1/field framing 加密，不回放桌面端的 Buffer 包装形态（我们的
// 读取路径两种都认）；哪天要写回给桌面端直接读的字段，再对齐包装形态。
func sealWBValue(v any) (map[string]any, error) {
	text, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
		}
		text = string(b)
	}
	key := sha256.Sum256([]byte(wbAtRestSecretKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("at_rest_cipher_failed: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("at_rest_gcm_failed: %w", err)
	}
	keyID := wbKeyID(key)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
	}
	ct := gcm.Seal(nil, nonce, []byte(text), wbFieldAAD(1, keyID))
	env, err := json.Marshal(map[string]any{
		"suite":      1,
		"keyId":      keyID,
		"nonce":      base64.StdEncoding.EncodeToString(nonce),
		"authTag":    base64.StdEncoding.EncodeToString(ct[len(ct)-16:]),
		"ciphertext": base64.StdEncoding.EncodeToString(ct[:len(ct)-16]),
	})
	if err != nil {
		return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
	}
	return map[string]any{
		"$wbEncrypted": 1,
		"envelope":     base64.StdEncoding.EncodeToString(env),
	}, nil
}

func wbUnwrapBuffer(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t["type"] == "Buffer" {
			if arr, ok := t["data"].([]any); ok {
				buf := make([]byte, 0, len(arr))
				for _, n := range arr {
					f, ok := n.(float64)
					if !ok {
						return t // 形状不符就不解，原样留着让上层看到
					}
					buf = append(buf, byte(f))
				}
				return string(buf)
			}
		}
		for k, item := range t {
			t[k] = wbUnwrapBuffer(item)
		}
		return t
	case []any:
		for i, item := range t {
			t[i] = wbUnwrapBuffer(item)
		}
		return t
	}
	return v
}
