package signingkeys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// 证书参数。
const (
	// DefaultCAYears 是私有 CA 的默认有效期；可配范围 [MinCAYears, MaxCAYears]（ADR-003 §3）。
	DefaultCAYears = 10
	MinCAYears     = 5
	MaxCAYears     = 10
	// LeafValidity 是 Server gRPC 叶证书的有效期（ADR-003 §3：90 天；2/3 周期轮换属 Story 3.10）。
	LeafValidity = 90 * 24 * time.Hour
	// clockSkew 是 NotBefore 相对签发时刻的回拨量：容忍节点与 Server 之间的时钟偏差（证书校验是 TLS 层的，与配置签名「不检查过期」无关）。
	clockSkew = 5 * time.Minute

	caCommonName = "LingEdge Node CA"
)

func randSerial() (*big.Int, error) {
	// 128 位随机序列号（RFC 5280 要求正数且不超过 20 字节）。
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

func skid(pub *ecdsa.PublicKey) ([]byte, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(spki)
	return sum[:20], nil
}

// newCA 生成自签的 ECDSA P-256 私有 CA（IsCA、pathlen=0：只能签发叶证书，不能再签发中间 CA；KeyUsage=CertSign|CRLSign）。
func newCA(now time.Time, years int) (*ecdsa.PrivateKey, *x509.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("signingkeys: 生成 CA 密钥: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	id, err := skid(&key.PublicKey)
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName, Organization: []string{"LingEdge"}},
		NotBefore:             now.Add(-clockSkew).UTC(),
		NotAfter:              now.AddDate(years, 0, 0).UTC(),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		SubjectKeyId:          id,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("signingkeys: 签发 CA 证书: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	return key, cert, der, nil
}

// newServerLeaf 用 CA 签发 Server gRPC 叶证书：ECDSA P-256，ExtKeyUsage=serverAuth，SAN = hosts（IP 字面量进 IPAddresses，其余进 DNSNames），有效期 90 天。
// 叶证书不得超过 CA 的有效期（CA 最短 5 年，90 天恒在其内；这里仍做显式校验）。
func newServerLeaf(now time.Time, ca *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string) (*ecdsa.PrivateKey, *x509.Certificate, []byte, error) {
	if len(hosts) == 0 {
		return nil, nil, nil, errors.New("signingkeys: 叶证书至少需要一个 SAN")
	}
	notAfter := now.Add(LeafValidity).UTC()
	if notAfter.After(ca.NotAfter) {
		return nil, nil, nil, errors.New("signingkeys: 叶证书有效期超出 CA 有效期")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("signingkeys: 生成叶证书密钥: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	var dns []string
	var ips []net.IP
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, h)
		}
	}
	id, err := skid(&key.PublicKey)
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0], Organization: []string{"LingEdge"}},
		NotBefore:    now.Add(-clockSkew).UTC(),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
		SubjectKeyId: id,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("signingkeys: 签发叶证书: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	return key, cert, der, nil
}

func certPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func parseCertPEM(s string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(s))
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("signingkeys: 证书 PEM 非法")
	}
	return x509.ParseCertificate(b.Bytes)
}
