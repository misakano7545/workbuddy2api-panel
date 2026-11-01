// decode_body_test.go 响应体解压的回归（Accept-Encoding 显式声明后的必需配套）。
//
// 触发条件：CommonHeaders 现在显式声明 `gzip, compress, deflate, br`，
// net/http 因此**不再透明解压**。若 decodeResponseBody 有洞，所有走 doJSON
// 的接口全部解析失败——这不是「一个头的优化」，是链路级前提。
package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

const decodeTestJSON = `{"code":0,"msg":"OK","data":{"hello":"world"}}`

// encodeWith 按 (header 声明的编码, 实际压缩方式) 造响应体。
//
// 注意 deflate 的坑：RFC 7230 说 `deflate` = zlib 包装，但相当多的服务端
// 直接发裸 flate。两种都必须能解，而 Header 里写的都是 `deflate`。
func encodeWith(t *testing.T, mode string) (headerEnc string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	switch mode {
	case "gzip":
		w := gzip.NewWriter(&buf)
		w.Write([]byte(decodeTestJSON))
		w.Close()
		return "gzip", buf.Bytes()
	case "br":
		w := brotli.NewWriter(&buf)
		w.Write([]byte(decodeTestJSON))
		w.Close()
		return "br", buf.Bytes()
	case "deflate": // zlib 包装
		w := zlib.NewWriter(&buf)
		w.Write([]byte(decodeTestJSON))
		w.Close()
		return "deflate", buf.Bytes()
	case "deflate-raw": // 裸 flate，Header 仍写 deflate
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		w.Write([]byte(decodeTestJSON))
		w.Close()
		return "deflate", buf.Bytes()
	default:
		return "", []byte(decodeTestJSON)
	}
}

// TestDoJSONDecodesAllEncodings 各种编码都必须解回同一个 JSON。
func TestDoJSONDecodesAllEncodings(t *testing.T) {
	for _, mode := range []string{"", "gzip", "br", "deflate", "deflate-raw"} {
		t.Run(mode, func(t *testing.T) {
			headerEnc, body := encodeWith(t, mode)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 断言请求确实带上了我们声明的 Accept-Encoding
				if got := r.Header.Get("Accept-Encoding"); got != "gzip, compress, deflate, br" {
					t.Errorf("Accept-Encoding=%q want gzip, compress, deflate, br", got)
				}
				if headerEnc != "" {
					w.Header().Set("Content-Encoding", headerEnc)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(body)
			}))
			defer srv.Close()

			c := New()
			c.ChatBaseCN = srv.URL
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/test", bytes.NewReader([]byte(`{}`)))
			c.CommonHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"})

			data, err := c.doJSON(req, &auth.Auth{UID: "u1", AccessToken: "at"})
			if err != nil {
				t.Fatalf("doJSON(%s) 失败: %v", mode, err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("解出的 data 不是 JSON: %v (%q)", err, string(data))
			}
			if got["hello"] != "world" {
				t.Fatalf("data=%v want hello=world", got)
			}
		})
	}
}

// TestDoJSONClassifiesDecodedErrorBody 400 分支必须看到**解压后**的文本，
// 否则 Classify 会按压缩字节分类，罚号策略全错。
func TestDoJSONClassifiesDecodedErrorBody(t *testing.T) {
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	w.Write([]byte(`{"code":402,"msg":"insufficient credit"}`))
	w.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	c := New()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/test", bytes.NewReader([]byte(`{}`)))
	c.CommonHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"})

	_, err := c.doJSON(req, &auth.Auth{UID: "u1", AccessToken: "at"})
	if err == nil {
		t.Fatal("402 应返回错误")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("错误类型 = %T want *Error", err)
	}
	if ue.Kind != ErrHardCredit {
		t.Fatalf("分类 = %v want hard_credit（说明 Classify 没看到解压后的文本）", ue.Kind)
	}
}

// TestDecodeBodyRejectsUnknownEncoding 未知编码必须明确报错，不能把压缩
// 字节当 JSON 往下传（那会变成"解析失败"这种误导性错误）。
func TestDecodeBodyRejectsUnknownEncoding(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Encoding", "compress") // LZW，官方声明但服务端不回
	if _, err := decodeResponseBody(resp, []byte("xx")); err == nil {
		t.Fatal("未知编码应报错")
	}
}
