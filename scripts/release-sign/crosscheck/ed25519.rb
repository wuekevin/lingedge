# frozen_string_literal: true
#
# Ed25519 交叉验证（Story 1.34 AC6、tools-design §9.6）：用 Ruby OpenSSL 独立于 Go crypto/ed25519 做签名/验签。
# 需要支持 Ed25519 的 OpenSSL（OpenSSL >= 1.1.1 / Ruby openssl gem >= 3.0）；macOS 系统自带 Ruby 2.6 + LibreSSL 3.3 不支持，
# 会以退出码 3 报告 "ed25519-unsupported"（调用方据此把它标为「等待环境验证」，而不是通过）。
#
# 协议（标准输入/输出均为 JSON，二进制一律标准 base64）：
#   {"op":"verify","pub_b64":"...","cases":[{"name":"x","input_b64":"...","sig_b64":"..."}]}  -> {"x":true|false,...}
#   {"op":"sign","seed_b64":"...","cases":[{"name":"x","input_b64":"..."}]}                   -> {"x":"<sig_b64>",...}
#   {"op":"probe"}                                                                           -> {"openssl":"<版本>","ed25519":true}
require "openssl"
require "json"

def b64d(s)
  s.unpack1("m")
end

def b64e(s)
  [s].pack("m0")
end

begin
  OpenSSL::PKey.new_raw_private_key("ED25519", "\x00" * 32)
rescue StandardError, NotImplementedError
  warn "ed25519-unsupported: #{OpenSSL::OPENSSL_LIBRARY_VERSION}"
  exit 3
end

req = JSON.parse($stdin.read)
out =
  case req["op"]
  when "probe"
    { "openssl" => OpenSSL::OPENSSL_LIBRARY_VERSION, "ed25519" => true }
  when "verify"
    pub = OpenSSL::PKey.new_raw_public_key("ED25519", b64d(req["pub_b64"]))
    req["cases"].to_h do |c|
      sig = b64d(c["sig_b64"])
      ok = sig.bytesize == 64 && pub.verify(nil, sig, b64d(c["input_b64"]))
      [c["name"], ok]
    rescue OpenSSL::PKey::PKeyError
      [c["name"], false]
    end
  when "sign"
    priv = OpenSSL::PKey.new_raw_private_key("ED25519", b64d(req["seed_b64"]))
    req["cases"].to_h { |c| [c["name"], b64e(priv.sign(nil, b64d(c["input_b64"])))] }
  else
    warn "unknown op"
    exit 2
  end
puts JSON.generate(out)
