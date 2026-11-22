require "db"
require "sqlite3"
require "uri"
require "http/server"
require "./mysql_auth"

class AppContext
  getter db : DB::Database
  getter dialect : String
  getter secret : String
  getter base_url : String
  getter allow_register : Bool
  getter auth_rate_limit : Int32

  @rate_lock = Mutex.new
  @rate_entries = {} of String => Tuple(Time::Span, Int32)

  def initialize
    @dialect = ENV.fetch("DATABASE_DIALECT", "sqlite")
    @secret = ENV["JWT_SECRET"]? || raise "JWT_SECRET must be set"
    raise "JWT_SECRET must contain at least 32 bytes" if @secret.bytesize < 32
    @base_url = ENV.fetch("BASE_URL", "http://localhost:8080").rstrip('/')
    @allow_register = ENV.fetch("ALLOW_NEW_REGISTER", "true") == "true"
    @auth_rate_limit = ENV.fetch("AUTH_RATE_LIMIT", "5").to_i
    raise "AUTH_RATE_LIMIT must be positive" if @auth_rate_limit <= 0

    @db = case @dialect
          when "sqlite"
            path = ENV.fetch("DATABASE_PATH", "kotatsu.sqlite")
            query = "journal_mode=WAL&synchronous=FULL&busy_timeout=5000&foreign_keys=on&max_pool_size=1"
            DB.open URI.new("sqlite3", path: URI.encode_www_form(path, space_to_plus: false), query: query)
          when "mysql"
            host = ENV.fetch("DATABASE_HOST", "localhost")
            port = ENV.fetch("DATABASE_PORT", "3306").to_i
            name = ENV.fetch("DATABASE_NAME", "kotatsu_db")
            user = ENV["DATABASE_USER"]? || raise "DATABASE_USER must be set"
            password = ENV["DATABASE_PASSWORD"]? || raise "DATABASE_PASSWORD must be set"
            # The local benchmark's MySQL uses a self-signed certificate. Remote
            # servers must present a certificate valid for their host name.
            mode = host.in?("localhost", "127.0.0.1", "::1") ? "Required" : "VerifyIdentity"
            uri = URI.new("mysql", host, port, "/#{name}", "ssl-mode=#{mode}&max_pool_size=8&max_idle_pool_size=8", user, password)
            DB.open uri
          else
            raise "Unsupported DATABASE_DIALECT #{@dialect}"
          end
    migrate_users
  end

  def allowed?(kind : String, ip : String, limit : Int32, period : Time::Span) : Bool
    now = Time.monotonic
    key = "#{kind}:#{ip}"
    @rate_lock.synchronize do
      if @rate_entries.size >= 10_000 && !@rate_entries.has_key?(key)
        @rate_entries.reject! { |_, (start, _)| now - start >= 10.minutes }
        return false if @rate_entries.size >= 10_000
      end
      start, count = @rate_entries[key]? || {now, 0}
      if now - start >= period
        start = now
        count = 0
      end
      return false if count >= limit
      @rate_entries[key] = {start, count + 1}
      true
    end
  end

  private def migrate_users
    if @dialect == "sqlite"
      @db.exec <<-SQL
        CREATE TABLE IF NOT EXISTS app_users (
          id INTEGER PRIMARY KEY,
          email TEXT NOT NULL UNIQUE,
          password_hash TEXT NOT NULL,
          token_version INTEGER NOT NULL DEFAULT 0,
          reset_hash TEXT,
          reset_expires INTEGER
        )
        SQL
    else
      @db.exec <<-SQL
        CREATE TABLE IF NOT EXISTS app_users (
          id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
          email VARCHAR(320) NOT NULL UNIQUE,
          password_hash VARCHAR(255) NOT NULL,
          token_version BIGINT NOT NULL DEFAULT 0,
          reset_hash VARCHAR(64),
          reset_expires BIGINT
        ) ENGINE=InnoDB
        SQL
    end
  end
end
