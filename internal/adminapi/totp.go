package adminapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 本文件实现 Google Authenticator 使用的 TOTP（RFC 6238，HMAC-SHA1，30s 步长，6 位），
// 避免引入额外第三方依赖。secret 以 RFC4648 base32（无填充、大写）表示。

const (
	totpStep   = 30 // 秒
	totpDigits = 6
	totpSecretBytes = 20 // 160-bit 密钥
)

// GenerateTOTPSecret 生成一个新的 base32（无填充）随机密钥。
func GenerateTOTPSecret() (string, error) {
	buf := make([]byte, totpSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// totpCounter 返回给定时间的步长计数器。
func totpCounter(t time.Time) uint64 {
	return uint64(t.Unix()) / uint64(totpStep)
}

// hmacSHA1 计算 HMAC-SHA1(secret, msg)。
func hmacSHA1(key, msg []byte) []byte {
	mac := hmac.New(sha1.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}

// totpCode 计算指定计数器下的 6 位动态码（动态截断算法见 RFC 6238/4226）。
func totpCode(secretBytes []byte, counter uint64) string {
	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, counter)
	hash := hmacSHA1(secretBytes, msg)
	offset := hash[len(hash)-1] & 0x0f
	binCode := (uint32(hash[offset])<<24 | uint32(hash[offset+1])<<16 |
		uint32(hash[offset+2])<<8 | uint32(hash[offset+3])) & 0x7fffffff
	code := binCode % uint32(pow10(totpDigits))
	return fmt.Sprintf("%0*d", totpDigits, code)
}

func pow10(n int) uint32 {
	v := uint32(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// TOTPCode 计算给定密钥与时间下的当前动态码（用于校验前端提交码或供测试）。
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	return totpCode(key, totpCounter(t)), nil
}

// VerifyTOTP 校验动态码，允许前后各一个步长（±30s）的时钟漂移。
func VerifyTOTP(secret, code string, t time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return false
	}
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return false
	}
	c := totpCounter(t)
	for _, cc := range []uint64{c - 1, c, c + 1} {
		if totpCode(key, cc) == code {
			return true
		}
	}
	return false
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	secret = strings.TrimSpace(strings.ToUpper(secret))
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
}

// OTPAuthURI 生成 otpauth:// 标准 URI（供前端生成二维码，Google Authenticator 扫码即用）。
func OTPAuthURI(issuer, account, secret string) string {
	issuer = url.QueryEscape(issuer)
	account = url.QueryEscape(account)
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&period=%d&digits=%d&algorithm=SHA1",
		issuer, account, strings.ToUpper(secret), issuer, totpStep, totpDigits)
}

