package backup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

// 加密容器格式（tools-design §17.1：口令经 argon2id 派生密钥，AES-256-GCM 分块加密）。
//
//	头部（明文，但整体作为每个分块的 AAD，被篡改会使首个分块认证失败）：
//	  magic "LEBK" + 版本 1 字节 | argon2id time u32 | memKiB u32 | threads u8 | salt 16B | nonce 前缀 4B | 分块明文大小 u32 | meta 长度 u32 | meta
//	分块（重复至 final）：
//	  flag u8（0 中间块 / 1 最后块）| 密文长度 u32 | 密文（含 16B GCM 标签）
//	  nonce = 前缀(4B) ‖ 分块序号 u64（大端）；AAD = 头部 ‖ flag。
//
// 设计要点：flag 在 AAD 里 → 截断（缺 final）、重排、重复分块、篡改 flag 都会认证失败或被识别为截断；
// 每个文件随机 salt → 同一口令得到不同密钥；meta 是一小段不含机密的 JSON（组件、版本、kek_ids_required、主密钥指纹…），
// 随头部一起被认证，使 `backup list` 与「移除 KEK 前的核对」无需口令即可读取，但篡改会在解密时暴露。

// 容器常量。
const (
	encMagic       = "LEBK"
	encVersion     = 1
	saltSize       = 16
	noncePrefix    = 4
	keySize        = 32
	gcmTag         = 16
	defaultChunk   = 64 << 10
	maxChunk       = 4 << 20
	maxMetaLen     = 16 << 10
	minPassphrase  = 10
	maxPassphrase  = 1024
	fixedHeaderLen = len(encMagic) + 1 + 4 + 4 + 1 + saltSize + noncePrefix + 4 + 4
)

// 错误类别。
var (
	// ErrBadPassphrase：口令错误，或文件头/首个分块被篡改（两者在密码学上不可区分）。
	ErrBadPassphrase = errors.New("backup: 口令错误，或备份文件已损坏/被篡改")
	// ErrCorrupt：加密流在认证通过之后仍然不完整或结构不合法（截断、分块重排、尾部多余数据）。
	ErrCorrupt = errors.New("backup: 加密备份已损坏（被截断、重排或含多余数据）")
	// ErrNotEncrypted：文件不是加密容器。
	ErrNotEncrypted = errors.New("backup: 不是加密备份")
	// ErrWeakPassphrase：口令太短。
	ErrWeakPassphrase = fmt.Errorf("backup: 口令至少 %d 个字符", minPassphrase)
)

// KDFParams 是 argon2id 参数。写入文件头，读取时按头部参数派生（并限制上限，防止恶意头部耗尽内存）。
type KDFParams struct {
	Time      uint32
	MemoryKiB uint32
	Threads   uint8
}

// DefaultKDF 是生产默认值（OWASP 建议量级：64 MiB、3 轮、4 线程）。
var DefaultKDF = KDFParams{Time: 3, MemoryKiB: 64 << 10, Threads: 4}

func (k KDFParams) valid() error {
	switch {
	case k.Time < 1 || k.Time > 10:
		return errors.New("backup: argon2id time 超出 1–10")
	case k.MemoryKiB < 8 || k.MemoryKiB > 1<<20:
		return errors.New("backup: argon2id memory 超出 8 KiB–1 GiB")
	case k.Threads < 1 || k.Threads > 16:
		return errors.New("backup: argon2id threads 超出 1–16")
	}
	return nil
}

// CheckPassphrase 校验创建加密备份时的口令强度下限（只检查长度；不回显口令）。
func CheckPassphrase(p []byte) error {
	n := len([]rune(string(p)))
	if n < minPassphrase {
		return ErrWeakPassphrase
	}
	if len(p) > maxPassphrase {
		return errors.New("backup: 口令过长")
	}
	return nil
}

type encHeader struct {
	kdf       KDFParams
	salt      [saltSize]byte
	nonce     [noncePrefix]byte
	chunkSize uint32
	meta      []byte
	raw       []byte // 头部原始字节（AAD）
}

func (h *encHeader) marshal() []byte {
	var b bytes.Buffer
	b.WriteString(encMagic)
	b.WriteByte(encVersion)
	_ = binary.Write(&b, binary.BigEndian, h.kdf.Time)
	_ = binary.Write(&b, binary.BigEndian, h.kdf.MemoryKiB)
	b.WriteByte(h.kdf.Threads)
	b.Write(h.salt[:])
	b.Write(h.nonce[:])
	_ = binary.Write(&b, binary.BigEndian, h.chunkSize)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(h.meta)))
	b.Write(h.meta)
	return b.Bytes()
}

func readHeader(r io.Reader) (*encHeader, error) {
	fixed := make([]byte, fixedHeaderLen)
	if _, err := io.ReadFull(r, fixed); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrNotEncrypted
		}
		return nil, err
	}
	if string(fixed[:len(encMagic)]) != encMagic {
		return nil, ErrNotEncrypted
	}
	if fixed[len(encMagic)] != encVersion {
		return nil, fmt.Errorf("backup: 不支持的加密容器版本 %d", fixed[len(encMagic)])
	}
	p := len(encMagic) + 1
	h := &encHeader{}
	h.kdf.Time = binary.BigEndian.Uint32(fixed[p:])
	h.kdf.MemoryKiB = binary.BigEndian.Uint32(fixed[p+4:])
	h.kdf.Threads = fixed[p+8]
	copy(h.salt[:], fixed[p+9:])
	copy(h.nonce[:], fixed[p+9+saltSize:])
	q := p + 9 + saltSize + noncePrefix
	h.chunkSize = binary.BigEndian.Uint32(fixed[q:])
	metaLen := binary.BigEndian.Uint32(fixed[q+4:])
	if err := h.kdf.valid(); err != nil {
		return nil, err
	}
	if h.chunkSize < 1024 || h.chunkSize > maxChunk {
		return nil, errors.New("backup: 加密容器分块大小不合法")
	}
	if metaLen > maxMetaLen {
		return nil, errors.New("backup: 加密容器 meta 过长")
	}
	h.meta = make([]byte, metaLen)
	if _, err := io.ReadFull(r, h.meta); err != nil {
		return nil, ErrCorrupt
	}
	h.raw = append(append([]byte(nil), fixed...), h.meta...)
	return h, nil
}

// PeekMeta 读取加密容器头部里的 meta（无需口令；内容在解密之前不被认证，只用于展示与 KEK 移除前的保守核对）。
func PeekMeta(r io.Reader) ([]byte, error) {
	h, err := readHeader(r)
	if err != nil {
		return nil, err
	}
	return h.meta, nil
}

func newGCM(pass []byte, h *encHeader) (cipher.AEAD, error) {
	key := argon2.IDKey(pass, h.salt[:], h.kdf.Time, h.kdf.MemoryKiB, h.kdf.Threads, keySize)
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

func chunkNonce(prefix [noncePrefix]byte, n uint64) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix[:])
	binary.BigEndian.PutUint64(nonce[noncePrefix:], n)
	return nonce
}

func chunkAAD(header []byte, flag byte) []byte {
	return append(append(make([]byte, 0, len(header)+1), header...), flag)
}

// EncryptWriter 把写入的明文分块加密后写到底层 writer。必须 Close（写出 final 块）。
type EncryptWriter struct {
	w      io.Writer
	gcm    cipher.AEAD
	h      *encHeader
	buf    []byte
	n      uint64
	closed bool
}

// NewEncryptWriter 写出头部并返回加密写入器。meta 须是不含机密的短 JSON（≤16 KiB）。chunk 为 0 取默认值。
func NewEncryptWriter(w io.Writer, pass []byte, kdf KDFParams, meta []byte, chunk uint32) (*EncryptWriter, error) {
	if err := CheckPassphrase(pass); err != nil {
		return nil, err
	}
	if err := kdf.valid(); err != nil {
		return nil, err
	}
	if len(meta) > maxMetaLen {
		return nil, errors.New("backup: meta 过长")
	}
	if chunk == 0 {
		chunk = defaultChunk
	}
	if chunk < 1024 || chunk > maxChunk {
		return nil, errors.New("backup: 分块大小不合法")
	}
	h := &encHeader{kdf: kdf, chunkSize: chunk, meta: append([]byte(nil), meta...)}
	if _, err := rand.Read(h.salt[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(h.nonce[:]); err != nil {
		return nil, err
	}
	h.raw = h.marshal()
	gcm, err := newGCM(pass, h)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(h.raw); err != nil {
		return nil, err
	}
	return &EncryptWriter{w: w, gcm: gcm, h: h, buf: make([]byte, 0, chunk)}, nil
}

func (e *EncryptWriter) flush(final bool) error {
	flag := byte(0)
	if final {
		flag = 1
	}
	ct := e.gcm.Seal(nil, chunkNonce(e.h.nonce, e.n), e.buf, chunkAAD(e.h.raw, flag))
	e.n++
	var hdr [5]byte
	hdr[0] = flag
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(ct)))
	if _, err := e.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := e.w.Write(ct); err != nil {
		return err
	}
	e.buf = e.buf[:0]
	return nil
}

// Write 实现 io.Writer。满块只在「还有更多数据」时才落出，最后一块留给 Close 标记为 final。
func (e *EncryptWriter) Write(p []byte) (int, error) {
	if e.closed {
		return 0, errors.New("backup: 加密写入器已关闭")
	}
	total := len(p)
	for len(p) > 0 {
		if len(e.buf) == int(e.h.chunkSize) {
			if err := e.flush(false); err != nil {
				return total - len(p), err
			}
		}
		room := int(e.h.chunkSize) - len(e.buf)
		if room > len(p) {
			room = len(p)
		}
		e.buf = append(e.buf, p[:room]...)
		p = p[room:]
	}
	return total, nil
}

// Close 写出 final 块。不关闭底层 writer。
func (e *EncryptWriter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	return e.flush(true)
}

// DecryptReader 逐块解密并认证。读完 final 块后返回 io.EOF；底层先于 final 结束 → ErrCorrupt。
type DecryptReader struct {
	r     io.Reader
	gcm   cipher.AEAD
	h     *encHeader
	n     uint64
	buf   []byte
	final bool
	err   error
}

// NewDecryptReader 读取头部、派生密钥并返回解密读取器，同时返回头部 meta（解密首块之后才被认证）。
func NewDecryptReader(r io.Reader, pass []byte) (*DecryptReader, []byte, error) {
	h, err := readHeader(r)
	if err != nil {
		return nil, nil, err
	}
	if len(pass) == 0 {
		return nil, nil, ErrBadPassphrase
	}
	gcm, err := newGCM(pass, h)
	if err != nil {
		return nil, nil, err
	}
	d := &DecryptReader{r: r, gcm: gcm, h: h}
	// 立即读取并认证首块：口令错误/头部被改在此处暴露，而不是在读到一半时。
	if err := d.next(); err != nil {
		return nil, nil, err
	}
	return d, h.meta, nil
}

func (d *DecryptReader) next() error {
	var hdr [5]byte
	if _, err := io.ReadFull(d.r, hdr[:]); err != nil {
		if d.n == 0 {
			return ErrBadPassphrase
		}
		return ErrCorrupt
	}
	flag := hdr[0]
	if flag > 1 {
		return d.fail()
	}
	ln := binary.BigEndian.Uint32(hdr[1:])
	if ln < gcmTag || ln > d.h.chunkSize+gcmTag {
		return d.fail()
	}
	ct := make([]byte, ln)
	if _, err := io.ReadFull(d.r, ct); err != nil {
		if d.n == 0 {
			return ErrBadPassphrase
		}
		return ErrCorrupt
	}
	pt, err := d.gcm.Open(d.buf[:0], chunkNonce(d.h.nonce, d.n), ct, chunkAAD(d.h.raw, flag))
	if err != nil {
		return d.fail()
	}
	d.buf = pt
	d.n++
	if flag == 1 {
		d.final = true
		var one [1]byte
		if k, _ := d.r.Read(one[:]); k > 0 {
			return ErrCorrupt
		}
	}
	return nil
}

func (d *DecryptReader) fail() error {
	if d.n == 0 {
		return ErrBadPassphrase
	}
	return ErrCorrupt
}

// Read 实现 io.Reader。
func (d *DecryptReader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.final {
			d.err = io.EOF
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			d.err = err
			return 0, err
		}
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}
