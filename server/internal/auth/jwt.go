package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// jwtClaims 是 JWT 的原始 claims（只映射平台用到的字段）。
type jwtClaims struct {
	Issuer   string          `json:"iss"`
	Subject  string          `json:"sub"`
	Audience json.RawMessage `json:"aud"` // string 或 []string
	Exp      int64           `json:"exp"`
	Nbf      int64           `json:"nbf"`
	Iat      int64           `json:"iat"`
	Email    string          `json:"email"`
	Groups   any             `json:"-"`
	Nonce    string          `json:"nonce"`
}

// rawClaims 保留任意 claim，便于按 TESTPLANE_ROLE_CLAIM 取 groups（含数组/字符串两种形态）。
type rawClaims map[string]any

// jwks 是 JWKS 文档（只取 RSA 签名 key）。
type jwks struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// parseRSAKey 把 JWK 的 n/e 还原成 *rsa.PublicKey（不引 JWKS 库）。
func parseRSAKey(k jwk) (*rsa.PublicKey, error) {
	if !strings.EqualFold(k.Kty, "RSA") {
		return nil, fmt.Errorf("auth: 不支持的 kty=%q", k.Kty)
	}
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("auth: 解析 n: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("auth: 解析 e: %w", err)
	}
	n := new(big.Int).SetBytes(nb)
	e := new(big.Int).SetBytes(eb)
	if n.Sign() <= 0 || e.Sign() <= 0 || !e.IsInt64() || e.Int64() > 1<<31-1 {
		return nil, errors.New("auth: JWK 的 n/e 非法")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// verifyRS256 校验 JWS 签名（只支持 RS256，其余 alg 一律拒绝，避免 alg 混淆）。
func verifyRS256(token string, key *rsa.PublicKey) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%w: JWT 段数不是 3", ErrUnauthenticated)
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("%w: header 解码失败", ErrUnauthenticated)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return fmt.Errorf("%w: header 不是 JSON", ErrUnauthenticated)
	}
	if header.Alg != "RS256" {
		return fmt.Errorf("%w: 不支持 alg=%q（只接受 RS256）", ErrUnauthenticated, header.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("%w: 签名解码失败", ErrUnauthenticated)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return fmt.Errorf("%w: 签名无效", ErrUnauthenticated)
	}
	return nil
}

// parseClaims 解析 payload 并做**时间与 issuer/audience** 校验。
// leeway 固定 60s（07 §2.1）。nonce 校验由调用方按场景做（浏览器流才要求）。
func parseClaims(token string, leeway time.Duration, now time.Time) (rawClaims, jwtClaims, error) {
	var c jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, c, fmt.Errorf("%w: JWT 段数不是 3", ErrUnauthenticated)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, c, fmt.Errorf("%w: payload 解码失败", ErrUnauthenticated)
	}
	raw := rawClaims{}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, c, fmt.Errorf("%w: payload 不是 JSON 对象", ErrUnauthenticated)
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, c, fmt.Errorf("%w: claims 解析失败", ErrUnauthenticated)
	}
	nowT := now
	if c.Exp == 0 {
		return nil, c, fmt.Errorf("%w: 缺少 exp", ErrUnauthenticated)
	}
	if nowT.After(time.Unix(c.Exp, 0).Add(leeway)) {
		return nil, c, fmt.Errorf("%w: token 已过期", ErrUnauthenticated)
	}
	if c.Nbf != 0 && nowT.Add(leeway).Before(time.Unix(c.Nbf, 0)) {
		return nil, c, fmt.Errorf("%w: token 尚未生效（nbf）", ErrUnauthenticated)
	}
	return raw, c, nil
}

// audienceOf 把 aud 的两种形态（string / []string）统一成切片。
func audienceOf(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

// groupsFrom 按 claim 名取 groups；字符串（空格/逗号分隔）与数组两种形态都接受。
func groupsFrom(raw rawClaims, claim string) []string {
	v, ok := raw[claim]
	if !ok || v == nil {
		return nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, it := range t {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case string:
		fields := strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == ' ' })
		out := make([]string, 0, len(fields))
		for _, f := range fields {
			if f != "" {
				out = append(out, f)
			}
		}
		return out
	default:
		return nil
	}
}
