require "http/server"
require "./sync_storage"

module CrystalSync
  private def self.json(context : HTTP::Server::Context, value) : Nil
    context.response.content_type = "application/json; charset=utf-8"
    value.to_json(context.response)
  end

  private def self.error(context : HTTP::Server::Context, status : Int32, message : String) : Nil
    context.response.status_code = status
    context.response.content_type = "text/plain; charset=utf-8"
    context.response.print(message)
  end

  def self.route(context : HTTP::Server::Context, app : AppContext, user_id : Int64?) : Bool
    request = context.request
    path = request.resource.split('?', 2)[0]
    while path.starts_with?("//")
      path = path[1..]
    end
    method = request.method

    case path
    when "/resource/history"
      return false unless method.in?("GET", "HEAD", "POST")
      unless user_id
        context.response.status_code = 401
        return true
      end
      input = method == "POST" ? HistoryInput.from_json(request.body || raise InvalidPackage.new("Missing body")) : nil
      rows, at = history(app.db, app.dialect, user_id, input)
      if input && rows == input.history
        context.response.status_code = 204
      else
        json(context, {history: rows, timestamp: at})
      end
    when "/resource/favourites"
      return false unless method.in?("GET", "HEAD", "POST")
      unless user_id
        context.response.status_code = 401
        return true
      end
      input = method == "POST" ? FavouritesInput.from_json(request.body || raise InvalidPackage.new("Missing body")) : nil
      categories, rows, at = favourites(app.db, app.dialect, user_id, input)
      if input && categories == input.categories && rows == input.favourites
        context.response.status_code = 204
      else
        json(context, {categories: categories, favourites: rows, timestamp: at})
      end
    when "/manga"
      return false unless method.in?("GET", "HEAD")
      offset = request.query_params["offset"]?.try(&.to_i32?)
      limit = request.query_params["limit"]?.try(&.to_i32?)
      if offset.nil? || limit.nil? || offset < 0 || limit < 0
        error(context, 400, "Invalid offset or limit")
      else
        json(context, catalog(app.db, offset, limit))
      end
    else
      return false unless path.starts_with?("/manga/") && method.in?("GET", "HEAD")
      id = path[7..].to_i64?
      card = id ? catalog_item(app.db, id) : nil
      if card
        json(context, card)
      else
        error(context, 404, "Not Found")
      end
    end
    true
  rescue ex : JSON::ParseException | InvalidPackage | ArgumentError
    error(context, 400, "Invalid sync package")
    true
  end
end
