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
  getter api_rate_limit : Int32
  getter auth_rate_limit : Int32
  getter forgot_rate_limit : Int32
  getter reset_rate_limit : Int32

  @rate_lock = Mutex.new
  @rate_entries = {} of String => Tuple(Time::Instant, Int32)
  @password_lock = Mutex.new
  @password_limit : Int32
  @password_active = 0
  @api_start = Time.instant
  @api_count = 0

  def initialize
    @dialect = ENV.fetch("DATABASE_DIALECT", "sqlite")
    @secret = ENV["JWT_SECRET"]? || raise "JWT_SECRET must be set"
    raise "JWT_SECRET must contain at least 32 bytes" if @secret.bytesize < 32
    @base_url = ENV.fetch("BASE_URL", "http://localhost:8080").rstrip('/')
    @allow_register = ENV.fetch("ALLOW_NEW_REGISTER", "true") == "true"
    @api_rate_limit = positive_env("RATE_LIMIT_API_PER_SECOND", 200)
    @auth_rate_limit = positive_env("RATE_LIMIT_AUTH_PER_MINUTE", 5)
    @forgot_rate_limit = positive_env("RATE_LIMIT_FORGOT_PER_10_MINUTES", 5)
    @reset_rate_limit = positive_env("RATE_LIMIT_RESET_PER_10_MINUTES", 5)
    @password_limit = positive_env("RATE_LIMIT_PASSWORD_CONCURRENCY", 2)

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

  # Returns the remaining window in seconds when rejected, otherwise nil.
  def api_retry_after : Int32?
    now = Time.instant
    @rate_lock.synchronize do
      if now - @api_start >= 1.second
        @api_start = now
        @api_count = 0
      end
      return retry_seconds(1.second - (now - @api_start)) if @api_count >= @api_rate_limit
      @api_count += 1
      nil
    end
  end

  def retry_after(kind : String, ip : String, limit : Int32, period : Time::Span) : Int32?
    now = Time.instant
    key = "#{kind}:#{ip}"
    @rate_lock.synchronize do
      if @rate_entries.size >= 10_000 && !@rate_entries.has_key?(key)
        @rate_entries.reject! { |_, (start, _)| now - start >= 10.minutes }
        return 1 if @rate_entries.size >= 10_000
      end
      start, count = @rate_entries[key]? || {now, 0}
      if now - start >= period
        start = now
        count = 0
      end
      return retry_seconds(period - (now - start)) if count >= limit
      @rate_entries[key] = {start, count + 1}
      nil
    end
  end

  def acquire_password : Bool
    @password_lock.synchronize do
      return false if @password_active >= @password_limit
      @password_active += 1
      true
    end
  end

  def release_password : Nil
    @password_lock.synchronize { @password_active -= 1 }
  end

  private def retry_seconds(wait : Time::Span) : Int32
    {wait.total_seconds.ceil.to_i, 1}.max
  end

  private def positive_env(name : String, default : Int32) : Int32
    value = ENV.fetch(name, default.to_s).to_i?
    raise "#{name} must be a positive integer" unless value && value > 0
    value
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
