require "json"
require "base64"
require "digest/sha256"
require "crypto/subtle"
require "random/secure"
require "openssl/hmac"
require "openssl"
require "socket"

@[Link("argon2")]
lib LibArgon2
  fun argon2id_hash_encoded(t_cost : UInt32, m_cost : UInt32, parallelism : UInt32,
                            password : UInt8*, password_len : LibC::SizeT,
                            salt : UInt8*, salt_len : LibC::SizeT, hash_len : LibC::SizeT,
                            encoded : UInt8*, encoded_len : LibC::SizeT) : Int32
  fun argon2id_verify(encoded : UInt8*, password : UInt8*, password_len : LibC::SizeT) : Int32
end

module PasswordHash
  extend self

  # Restrict verification to the one supported PHC profile before invoking libargon2.
  FORMAT = /\A\$argon2id\$v=19\$m=65536,t=3,p=4\$[A-Za-z0-9+\/]{22}\$[A-Za-z0-9+\/]{43}\z/

  def create(password : String) : String
    salt = Bytes.new(16)
    Random::Secure.random_bytes(salt)
    encoded = Bytes.new(128, 0_u8)
    result = LibArgon2.argon2id_hash_encoded(3, 65536, 4, password.to_unsafe, password.bytesize,
      salt.to_unsafe, salt.size, 32, encoded.to_unsafe, encoded.size)
    raise "Argon2 password hashing failed (#{result})" unless result == 0
    String.new(encoded.to_unsafe)
  end

  def verify(encoded : String, password : String) : Bool
    return false unless FORMAT.matches?(encoded)
    LibArgon2.argon2id_verify(encoded.to_unsafe, password.to_unsafe, password.bytesize) == 0
  end
end

module Auth
  extend self

  ISSUER = "http://0.0.0.0:8080/"
  AUDIENCE = "http://0.0.0.0:8080/resource"

  private def json_body(context : HTTP::Server::Context) : JSON::Any
    body = context.request.body || raise ArgumentError.new("Missing body")
    json = JSON.parse(body)
    raise ArgumentError.new("Expected object") unless json.as_h?
    json
  end

  private def field(json : JSON::Any, name : String) : String
    json[name]?.try(&.as_s?) || raise ArgumentError.new("Missing #{name}")
  end

  private def utf16_length(value : String) : Int32
    value.each_char.sum { |char| char.ord < 0x10000 ? 1 : 2 }
  end

  private def text(context : HTTP::Server::Context, status : Int32, body : String)
    context.response.status_code = status
    context.response.content_type = "text/plain; charset=utf-8"
    context.response.print(body)
  end

  private def with_password_gate(context : HTTP::Server::Context, app : AppContext, &block : -> T) : T? forall T
    unless app.acquire_password
      context.response.headers["Retry-After"] = "1"
      text(context, 503, "Password service is busy")
      return nil
    end
    result = Channel(Tuple(T?, Exception?)).new(1)
    begin
      Fiber::ExecutionContext::Isolated.new("password-hash") do
        begin
          result.send({block.call, nil})
        rescue ex
          result.send({nil, ex})
        ensure
          app.release_password
        end
      end
    rescue ex
      app.release_password
      raise ex
    end
    value, error = result.receive
    raise error if error
    value
  end

  private def token(app : AppContext, user_id : Int64, version : Int64) : String
    header = Base64.urlsafe_encode(%({"alg":"HS256","typ":"JWT"}), padding: false)
    payload = {
      "iss" => ISSUER, "aud" => AUDIENCE, "user_id" => user_id,
      "ver" => version, "exp" => Time.utc.to_unix + 30.days.total_seconds.to_i64,
    }.to_json
    input = "#{header}.#{Base64.urlsafe_encode(payload, padding: false)}"
    signature = OpenSSL::HMAC.digest(OpenSSL::Algorithm::SHA256, app.secret, input)
    "#{input}.#{Base64.urlsafe_encode(signature, padding: false)}"
  end

  def user_id(context : HTTP::Server::Context, app : AppContext) : Int64?
    authorization = context.request.headers["Authorization"]?
    return nil unless authorization && authorization.starts_with?("Bearer ")
    pieces = authorization[7..].split('.')
    return nil unless pieces.size == 3
    input = "#{pieces[0]}.#{pieces[1]}"
    signature = Base64.urlsafe_encode(OpenSSL::HMAC.digest(OpenSSL::Algorithm::SHA256, app.secret, input), padding: false)
    return nil unless Crypto::Subtle.constant_time_compare(signature, pieces[2])
    header = JSON.parse(Base64.decode_string(pieces[0]))
    claims = JSON.parse(Base64.decode_string(pieces[1]))
    return nil unless header.as_h? && claims.as_h?
    return nil unless header["alg"]?.try(&.as_s?) == "HS256"
    return nil unless claims["iss"]?.try(&.as_s?) == ISSUER && claims["aud"]?.try(&.as_s?) == AUDIENCE
    expires = claims["exp"]?.try(&.as_i64?) || return nil
    return nil if expires <= Time.utc.to_unix
    id = claims["user_id"]?.try(&.as_i64?) || return nil
    version = claims["ver"]?.try(&.as_i64?) || return nil
    actual = app.db.query_one?("SELECT token_version FROM app_users WHERE id = ?", id, as: Int64)
    actual == version ? id : nil
  rescue Base64::Error | JSON::ParseException | ArgumentError
    nil
  end

  def route(context : HTTP::Server::Context, app : AppContext, user_id : Int64?) : Bool
    request = context.request
    path = request.path
    if request.method == "GET" && path == "/me"
      if id = user_id
        row = app.db.query_one?("SELECT email FROM app_users WHERE id = ?", id, as: String)
        if row
          context.response.content_type = "application/json"
          context.response.print({"id" => id, "email" => row, "nickname" => nil}.to_json)
        else
          context.response.status_code = 401
        end
      else
        context.response.status_code = 401
      end
      return true
    end
    return false unless request.method == "POST" && path.in?("/auth", "/forgot-password", "/reset-password")
    ip = context.remote_address.try { |addr| addr.as?(Socket::IPAddress).try(&.address) } || "unknown"
    limit = case path
            when "/auth"            then app.auth_rate_limit
            when "/forgot-password" then app.forgot_rate_limit
            else                         app.reset_rate_limit
            end
    period = path == "/auth" ? 1.minute : 10.minutes
    if retry_after = app.retry_after(path, ip, limit, period)
      context.response.status_code = 429
      context.response.headers["Retry-After"] = retry_after.to_s
      return true
    end
    json = json_body(context)
    case path
    when "/auth"            then login(context, app, json)
    when "/forgot-password" then forgot(context, app, json)
    when "/reset-password"  then reset(context, app, json)
    end
    true
  end

  private def login(context : HTTP::Server::Context, app : AppContext, json : JSON::Any)
    email = field(json, "email")
    password = field(json, "password")
    unless (2..24).includes?(utf16_length(password))
      text(context, 400, "Password should be from 2 to 24 characters long")
      return
    end
    unless (5..320).includes?(utf16_length(email)) && email.includes?('@') && !email.includes?('\r') && !email.includes?('\n')
      text(context, 400, "Invalid email address")
      return
    end
    sql = "SELECT id, password_hash, token_version FROM app_users WHERE email = ?"
    user = app.db.query_one?(sql, email, as: {Int64, String, Int64})
    generated_hash = nil
    if user.nil? && app.allow_register
      generated_hash = with_password_gate(context, app) { PasswordHash.create(password) }
      return unless generated_hash
      insert = app.dialect == "sqlite" ? "INSERT OR IGNORE" : "INSERT IGNORE"
      app.db.exec "#{insert} INTO app_users (email, password_hash) VALUES (?, ?)", email, generated_hash
      user = app.db.query_one?(sql, email, as: {Int64, String, Int64})
    end
    # Skip a second derivation only if the account still has the hash just created.
    verified = if user && user[1] != generated_hash
                 with_password_gate(context, app) { PasswordHash.verify(user.not_nil![1], password) }
               else
                 !!user
               end
    return if verified.nil?
    unless user && verified
      text(context, 400, "Wrong password")
      return
    end
    context.response.content_type = "application/json"
    context.response.print({"token" => token(app, user[0], user[2])}.to_json)
  end

  private def forgot(context : HTTP::Server::Context, app : AppContext, json : JSON::Any)
    email = field(json, "email")
    bytes = Bytes.new(32)
    Random::Secure.random_bytes(bytes)
    reset_token = Base64.urlsafe_encode(bytes, padding: false)
    digest = Digest::SHA256.hexdigest(reset_token)
    now = Time.utc.to_unix
    result = app.db.exec(
      "UPDATE app_users SET reset_hash = ?, reset_expires = ? WHERE email = ? AND (reset_hash IS NULL OR reset_expires <= ?)",
      digest, now + 900, email, now)
    if result.rows_affected > 0
      link = "#{app.base_url}/deeplink/reset-password?token=#{reset_token}"
      begin
        send_mail(email, link)
      rescue ex
        app.db.exec "UPDATE app_users SET reset_hash = NULL, reset_expires = NULL WHERE reset_hash = ?", digest
        raise ex
      end
    end
    text(context, 200, "A password reset email was sent")
  end

  private def reset(context : HTTP::Server::Context, app : AppContext, json : JSON::Any)
    reset_token = field(json, "reset_token")
    password = field(json, "password")
    digest = Digest::SHA256.hexdigest(reset_token)
    now = Time.utc.to_unix
    id = app.db.query_one?("SELECT id FROM app_users WHERE reset_hash = ? AND reset_expires >= ?", digest, now, as: Int64)
    unless id
      text(context, 400, "Invalid or expired token")
      return
    end
    unless (2..24).includes?(utf16_length(password))
      text(context, 400, "Password should be from 2 to 24 characters long")
      return
    end
    hash = with_password_gate(context, app) { PasswordHash.create(password) }
    return unless hash
    updated = app.db.exec("UPDATE app_users SET password_hash = ?, reset_hash = NULL, reset_expires = NULL, token_version = token_version + 1 WHERE id = ? AND reset_hash = ? AND reset_expires >= ?", hash, id, digest, Time.utc.to_unix)
    if updated.rows_affected == 0
      text(context, 400, "Invalid or expired token")
    else
      text(context, 200, "Password has been reset successfully")
    end
  end

  private def smtp_reply(io : IO, expected : Int32) : String
    response = String.build do |buffer|
      loop do
        line = io.gets || raise IO::Error.new("SMTP connection closed")
        raise IO::Error.new("Invalid SMTP response") if line.size < 3
        raise IO::Error.new("SMTP rejected command: #{line[0, 3]}") unless line[0, 3].to_i? == expected
        buffer << line << '\n'
        break unless line.size > 3 && line[3] == '-'
      end
    end
    response
  end

  private def smtp_command(io : IO, command : String, expected : Int32) : String
    io << command << "\r\n"
    io.flush
    smtp_reply(io, expected)
  end

  private def send_mail(to : String, link : String)
    provider = ENV.fetch("MAIL_PROVIDER", "console")
    if provider == "console"
      puts "Password reset for #{to}: #{link}"
      return
    end
    raise "Unsupported MAIL_PROVIDER" unless provider == "smtp"
    host = ENV["SMTP_HOST"]? || raise "SMTP_HOST must be set"
    port = ENV.fetch("SMTP_PORT", "587").to_i
    username = ENV.fetch("SMTP_USERNAME", "")
    password = ENV.fetch("SMTP_PASSWORD", "")
    from = ENV["SMTP_FROM"]? || raise "SMTP_FROM must be set"
    raise "Invalid SMTP address" if {from, to}.any? { |s| s.includes?('\r') || s.includes?('\n') }
    envelope_from = if left = from.rindex('<')
                      right = from.rindex('>') || raise "Invalid SMTP_FROM"
                      from[left + 1...right]
                    else
                      from
                    end
    socket = TCPSocket.new(host, port)
    io : IO = socket
    tls = false
    begin
      if port == 465
        io = OpenSSL::SSL::Socket::Client.new(socket, hostname: host, sync_close: true)
        tls = true
      end
      smtp_reply(io, 220)
      capabilities = smtp_command(io, "EHLO localhost", 250)
      if !tls && capabilities.upcase.includes?("STARTTLS")
        smtp_command(io, "STARTTLS", 220)
        io = OpenSSL::SSL::Socket::Client.new(socket, hostname: host, sync_close: true)
        tls = true
        capabilities = smtp_command(io, "EHLO localhost", 250)
      end
      unless username.empty?
        raise "SMTP authentication requires TLS" unless tls
        if capabilities.upcase.includes?("PLAIN")
          smtp_command(io, "AUTH PLAIN #{Base64.strict_encode("\0#{username}\0#{password}")}", 235)
        else
          smtp_command(io, "AUTH LOGIN", 334)
          smtp_command(io, Base64.strict_encode(username), 334)
          smtp_command(io, Base64.strict_encode(password), 235)
        end
      end
      smtp_command(io, "MAIL FROM:<#{envelope_from}>", 250)
      smtp_command(io, "RCPT TO:<#{to}>", 250)
      smtp_command(io, "DATA", 354)
      io << "From: #{from}\r\nTo: #{to}\r\nSubject: Password reset\r\n"
      io << "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"
      io << "You can reset your password at #{link}\r\n.\r\n"
      io.flush
      smtp_reply(io, 250)
      smtp_command(io, "QUIT", 221)
    ensure
      io.close
    end
  end
end
