package user

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// TOTP 参数（RFC 6238 / Google Authenticator 兼容）。
const (
	totpPeriod = 30 // 秒
	totpDigits = 6
	totpSkew   = 1 // 允许前后各 1 个时间窗容错（网络/时钟漂移）
)

// newTFASecret 生成一个新的 base32 编码 TOTP 密钥（160 bit）。
func newTFASecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// tfaURI 生成 otpauth:// URI，便于前端生成二维码。
func tfaURI(issuer, account, secret string) string {
	account = strings.ReplaceAll(account, ":", "_")
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&period=%d&digits=%d",
		urlEncode(issuer), urlEncode(account), secret, urlEncode(issuer), totpPeriod, totpDigits)
}

// tfaNow 计算当前时间窗的 TOTP 码。
func tfaNow(secret string) (string, error) {
	counter := time.Now().Unix() / totpPeriod
	return tfaCode(secret, counter)
}

// tfaVerify 校验用户提交的码，允许 +/- totpSkew 个时间窗。
func tfaVerify(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return false
	}
	counter := time.Now().Unix() / totpPeriod
	for d := -totpSkew; d <= totpSkew; d++ {
		got, err := tfaCode(secret, counter+int64(d))
		if err != nil {
			return false
		}
		if subtleEqual(got, code) {
			return true
		}
	}
	return false
}

func tfaCode(secret string, counter int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		// 容错：部分实现带 '=' 填充
		key, err = base32.StdEncoding.DecodeString(secret)
		if err != nil {
			return "", err
		}
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf)
	sum := mac.Sum(nil)
	offset := sum[19] & 0x0f
	binCode := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	code := binCode % 1000000
	return fmt.Sprintf("%06d", code), nil
}

// subtleEqual 做常数时间比较，避免计时侧信道。
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func urlEncode(s string) string {
	s = strings.ReplaceAll(s, " ", "%20")
	s = strings.ReplaceAll(s, "&", "%26")
	s = strings.ReplaceAll(s, "=", "%3D")
	return s
}
