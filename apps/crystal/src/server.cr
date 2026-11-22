require "http"
require "html"
require "json"
require "./app"
require "./auth"
require "./sync"

class KotatsuServer
  def initialize(@app : AppContext)
  end

  def call(context : HTTP::Server::Context) : Nil
    request = context.request
    # Android clients can append a slash to a base URL already ending in one.
    # Normalize the raw target before URI parsing treats // as an authority.
    raw_path = request.resource.split('?', 2)[0]
    request.path = "/#{raw_path.lstrip('/')}" if raw_path.starts_with?("//")
    head = request.method == "HEAD"
    if head
      request.method = "GET"
      output = context.response.output
      buffer = IO::Memory.new
      context.response.output = buffer
      begin
        handle(context)
      ensure
        context.response.output = output
        context.response.content_length = buffer.size unless context.response.status_code == 204
      end
    else
      handle(context)
    end
  end

  private def handle(context : HTTP::Server::Context) : Nil
    dispatch(context)
  rescue ex : JSON::ParseException | ArgumentError | TypeCastError | KeyError
    context.response.status_code = 400
    context.response.content_type = "text/plain; charset=utf-8"
    context.response.print("400: #{ex.message}")
  rescue ex
    STDERR.puts "Request failed: #{ex.message}"
    context.response.status_code = 500
    context.response.content_type = "text/plain; charset=utf-8"
    context.response.print("Internal Server Error")
  end

  private def dispatch(context : HTTP::Server::Context) : Nil
    request = context.request
    if request.method == "GET" && request.path == "/"
      context.response.content_type = "text/plain; charset=utf-8"
      context.response.print("Alive")
      return
    end
    if request.method == "GET" && request.path == "/deeplink/reset-password"
      token = request.query_params["token"]?
      if token.nil? || token.strip.empty?
        context.response.status_code = 400
        context.response.content_type = "text/plain; charset=utf-8"
        context.response.print("Missing token")
      else
        link = "kotatsu://reset-password?base_url=#{URI.encode_www_form(@app.base_url)}&token=#{URI.encode_www_form(token)}"
        escaped = HTML.escape(link)
        context.response.content_type = "text/html; charset=utf-8"
        context.response.print("<!DOCTYPE html><html lang=\"en\"><head><meta charset=\"UTF-8\"><title>Reset password</title><script>window.location.href = #{link.to_json};</script></head><body><main><p>If you are not redirected automatically, <a href=\"#{escaped}\">click here</a>.</p></main></body></html>")
      end
      return
    end
    path = request.path
    method = request.method
    api_route = (method == "POST" && path.in?("/auth", "/forgot-password", "/reset-password")) ||
                (method == "GET" && (path.in?("/me", "/manga") || path.starts_with?("/manga/"))) ||
                (method.in?("GET", "POST") && path.in?("/resource/history", "/resource/favourites"))
    if api_route
      if retry_after = @app.api_retry_after
        context.response.status_code = 429
        context.response.headers["Retry-After"] = retry_after.to_s
        return
      end
    end
    user_id = Auth.user_id(context, @app)
    return if Auth.route(context, @app, user_id)
    return if CrystalSync.route(context, @app, user_id)
    context.response.status_code = 404
  end
end

app = AppContext.new
CrystalSync.migrate(app.db, app.dialect)
handler = KotatsuServer.new(app)
server = HTTP::Server.new([HTTP::CompressHandler.new] of HTTP::Handler) do |context|
  handler.call(context)
end
server.bind_tcp("127.0.0.1", ENV.fetch("PORT", "8080").to_i)
server.listen
