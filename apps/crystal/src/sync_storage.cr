require "db"
require "./sync_models"

module CrystalSync
  def self.migrate(db : DB::Database, dialect : String) : Nil
    db.exec <<-SQL
      CREATE TABLE IF NOT EXISTS sync_state (
        user_id BIGINT PRIMARY KEY, history_at BIGINT NOT NULL, favourites_at BIGINT NOT NULL
      )
      SQL
    db.exec <<-SQL
      CREATE TABLE IF NOT EXISTS sync_manga (id BIGINT PRIMARY KEY, payload LONGTEXT NOT NULL)
      SQL
    db.exec <<-SQL
      CREATE TABLE IF NOT EXISTS sync_tags (id BIGINT PRIMARY KEY, payload LONGTEXT NOT NULL)
      SQL
    db.exec <<-SQL
      CREATE TABLE IF NOT EXISTS sync_manga_tags (
        manga_id BIGINT NOT NULL, tag_id BIGINT NOT NULL, PRIMARY KEY (manga_id, tag_id)
      )
      SQL
    db.exec <<-SQL
      CREATE TABLE IF NOT EXISTS sync_history (
        user_id BIGINT NOT NULL, manga_id BIGINT NOT NULL, created_at BIGINT NOT NULL,
        updated_at BIGINT NOT NULL, chapter_id BIGINT NOT NULL, page INT NOT NULL,
        scroll FLOAT NOT NULL, percent FLOAT NOT NULL, chapters INT NOT NULL,
        deleted_at BIGINT NOT NULL, PRIMARY KEY (user_id, manga_id)
      )
      SQL
    db.exec <<-SQL
      CREATE TABLE IF NOT EXISTS sync_categories (
        user_id BIGINT NOT NULL, category_id INT NOT NULL, created_at BIGINT NOT NULL,
        sort_key INT NOT NULL, track INT NOT NULL, title VARCHAR(120) NOT NULL,
        sort_order VARCHAR(255) NOT NULL, deleted_at BIGINT NOT NULL, show_in_lib INT NOT NULL,
        PRIMARY KEY (user_id, category_id)
      )
      SQL
    db.exec <<-SQL
      CREATE TABLE IF NOT EXISTS sync_favourites (
        user_id BIGINT NOT NULL, manga_id BIGINT NOT NULL, category_id INT NOT NULL,
        sort_key INT NOT NULL, pinned INT NOT NULL, created_at BIGINT NOT NULL,
        deleted_at BIGINT NOT NULL, PRIMARY KEY (user_id, manga_id, category_id)
      )
      SQL
  end

  private def self.lock_user(conn : DB::Connection, dialect : String, user : Int64) : Nil
    sql = if dialect == "mysql"
            "INSERT INTO sync_state (user_id, history_at, favourites_at) VALUES (?, 0, 0) ON DUPLICATE KEY UPDATE user_id = VALUES(user_id)"
          else
            "INSERT INTO sync_state (user_id, history_at, favourites_at) VALUES (?, 0, 0) ON CONFLICT(user_id) DO UPDATE SET user_id = excluded.user_id"
          end
    # The first statement is a write: MySQL locks this user's row and SQLite
    # takes its writer lock before looking at any existing sync data.
    conn.exec(sql, user)
  end

  private def self.timestamp(conn : DB::Connection, user : Int64, history : Bool) : Int64
    column = history ? "history_at" : "favourites_at"
    conn.query_one?("SELECT #{column} FROM sync_state WHERE user_id = ?", user, as: Int64) || 0_i64
  end

  private def self.set_timestamp(conn : DB::Connection, user : Int64, history : Bool) : Nil
    column = history ? "history_at" : "favourites_at"
    conn.exec("UPDATE sync_state SET #{column} = ? WHERE user_id = ?", Time.utc.to_unix_ms, user)
  end

  private def self.truncate(value : String, length : Int32) : String
    value.size > length ? value[0, length] : value
  end

  private def self.normalized_manga(manga : Manga) : Manga
    result = manga.dup
    result.title = truncate(result.title, 100)
    result.alt_title = result.alt_title.try { |v| truncate(v, 100) }
    result.url = truncate(result.url, 255)
    result.public_url = truncate(result.public_url, 255)
    result.cover_url = truncate(result.cover_url, 255)
    result.large_cover_url = result.large_cover_url.try { |v| truncate(v, 255) } || ""
    result.author = result.author.try { |v| truncate(v, 64) }
    result.source = truncate(result.source, 32)
    result.content_rating = nil unless {"SAFE", "SUGGESTIVE", "ADULT"}.includes?(result.content_rating)
    result.state = nil unless {"ONGOING", "FINISHED", "ABANDONED", "PAUSED", "UPCOMING"}.includes?(result.state)
    result.tags = [] of MangaTag
    result
  end

  private def self.save_manga(conn : DB::Connection, dialect : String, manga : Manga) : Nil
    if dialect == "mysql"
      manga_sql = "INSERT INTO sync_manga (id, payload) VALUES (?, ?) ON DUPLICATE KEY UPDATE payload = VALUES(payload)"
      tag_sql = "INSERT INTO sync_tags (id, payload) VALUES (?, ?) ON DUPLICATE KEY UPDATE payload = VALUES(payload)"
      link_sql = "INSERT IGNORE INTO sync_manga_tags (manga_id, tag_id) VALUES (?, ?)"
    else
      manga_sql = "INSERT INTO sync_manga (id, payload) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET payload = excluded.payload"
      tag_sql = "INSERT INTO sync_tags (id, payload) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET payload = excluded.payload"
      link_sql = "INSERT INTO sync_manga_tags (manga_id, tag_id) VALUES (?, ?) ON CONFLICT(manga_id, tag_id) DO NOTHING"
    end
    conn.exec(manga_sql, manga.id, normalized_manga(manga).to_json)
    manga.tags.each do |tag|
      normalized = tag.dup
      normalized.title = truncate(tag.title, 64)
      normalized.key = truncate(tag.key, 120)
      normalized.source = truncate(tag.source, 32)
      conn.exec(tag_sql, tag.id, normalized.to_json)
      conn.exec(link_sql, manga.id, tag.id)
    end
  end

  private def self.hydrate_tags(conn : DB::Connection, manga : Array(Manga)) : Nil
    return if manga.empty?
    cards = Hash(Int64, Array(Manga)).new { |hash, id| hash[id] = [] of Manga }
    manga.each do |card|
      card.tags.clear
      cards[card.id] << card
    end
    ids = cards.keys.sort!
    ids.each_slice(500) do |batch|
      placeholders = (["?"] * batch.size).join(",")
      conn.query("SELECT mt.manga_id, t.payload FROM sync_manga_tags mt JOIN sync_tags t ON t.id = mt.tag_id WHERE mt.manga_id IN (#{placeholders}) ORDER BY mt.manga_id, mt.tag_id", args: batch) do |rs|
        rs.each do
          id = rs.read(Int64)
          tag = MangaTag.from_json(rs.read(String))
          cards[id].each { |card| card.tags << tag }
        end
      end
    end
  end

  private def self.load_history_clocks(conn : DB::Connection, user : Int64, rows : Array(History)) : Hash(Int64, Tuple(Int64, Int64))
    clocks = {} of Int64 => Tuple(Int64, Int64)
    ids = rows.map(&.manga_id)
    ids.uniq!
    ids.each_slice(500) do |batch|
      placeholders = (["?"] * batch.size).join(",")
      conn.query("SELECT manga_id, updated_at, deleted_at FROM sync_history WHERE user_id = ? AND manga_id IN (#{placeholders})", user, args: batch) do |rs|
        rs.each do
          id = rs.read(Int64)
          clocks[id] = {rs.read(Int64), rs.read(Int64)}
        end
      end
    end
    clocks
  end

  private def self.load_category_clocks(conn : DB::Connection, user : Int64, input : FavouritesInput) : Hash(Int32, Tuple(Int64, Int64))
    clocks = {} of Int32 => Tuple(Int64, Int64)
    ids = input.categories.map(&.id)
    input.favourites.each { |row| ids << row.category_id }
    ids.uniq!
    ids.each_slice(500) do |batch|
      placeholders = (["?"] * batch.size).join(",")
      conn.query("SELECT category_id, created_at, deleted_at FROM sync_categories WHERE user_id = ? AND category_id IN (#{placeholders})", user, args: batch) do |rs|
        rs.each do
          id = rs.read(Int32)
          clocks[id] = {rs.read(Int64), rs.read(Int64)}
        end
      end
    end
    clocks
  end

  private def self.load_favourite_clocks(conn : DB::Connection, user : Int64, rows : Array(Favourite)) : Hash(Tuple(Int64, Int32), Tuple(Int64, Int64))
    clocks = {} of Tuple(Int64, Int32) => Tuple(Int64, Int64)
    keys = rows.map { |row| {row.manga_id, row.category_id} }
    keys.uniq!
    keys.each_slice(200) do |batch|
      conditions = (["(manga_id = ? AND category_id = ?)"] * batch.size).join(" OR ")
      args = [] of Int64 | Int32
      batch.each do |key|
        args << key[0]
        args << key[1]
      end
      conn.query("SELECT manga_id, category_id, created_at, deleted_at FROM sync_favourites WHERE user_id = ? AND (#{conditions})", user, args: args) do |rs|
        rs.each do
          key = {rs.read(Int64), rs.read(Int32)}
          clocks[key] = {rs.read(Int64), rs.read(Int64)}
        end
      end
    end
    clocks
  end

  private def self.save_history(conn : DB::Connection, dialect : String, user : Int64, row : History,
                                clocks : Hash(Int64, Tuple(Int64, Int64))) : Nil
    old = clocks[row.manga_id]?
    if old
      updated, deleted = old
      return if row.updated_at < updated
      return if row.updated_at == updated && deleted != 0 && (row.deleted_at == 0 || row.deleted_at < deleted)
      return if deleted != 0 && row.deleted_at == 0 && row.updated_at <= deleted
    end
    sql = if dialect == "mysql"
            "INSERT INTO sync_history (user_id, manga_id, created_at, updated_at, chapter_id, page, scroll, percent, chapters, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE created_at=VALUES(created_at), updated_at=VALUES(updated_at), chapter_id=VALUES(chapter_id), page=VALUES(page), scroll=VALUES(scroll), percent=VALUES(percent), chapters=VALUES(chapters), deleted_at=VALUES(deleted_at)"
          else
            "INSERT INTO sync_history (user_id, manga_id, created_at, updated_at, chapter_id, page, scroll, percent, chapters, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(user_id, manga_id) DO UPDATE SET created_at=excluded.created_at, updated_at=excluded.updated_at, chapter_id=excluded.chapter_id, page=excluded.page, scroll=excluded.scroll, percent=excluded.percent, chapters=excluded.chapters, deleted_at=excluded.deleted_at"
          end
    conn.exec(sql, user, row.manga_id, row.created_at, row.updated_at, row.chapter_id, row.page, row.scroll, row.percent, row.chapters, row.deleted_at)
    clocks[row.manga_id] = {row.updated_at, row.deleted_at}
  end

  private def self.accept_library(old_created : Int64, old_deleted : Int64, created : Int64, deleted : Int64) : Bool
    return true if (old_deleted == 0) == (deleted == 0)
    old_event = Math.max(old_created, old_deleted)
    event = Math.max(created, deleted)
    event == old_event ? deleted != 0 : event > old_event
  end

  private def self.save_category(conn : DB::Connection, dialect : String, user : Int64, row : Category,
                                 clocks : Hash(Int32, Tuple(Int64, Int64))) : Nil
    old = clocks[row.id]?
    return if old && !accept_library(old[0], old[1], row.created_at, row.deleted_at)
    sql = if dialect == "mysql"
            "INSERT INTO sync_categories (user_id, category_id, created_at, sort_key, track, title, sort_order, deleted_at, show_in_lib) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE created_at=VALUES(created_at), sort_key=VALUES(sort_key), track=VALUES(track), title=VALUES(title), sort_order=VALUES(sort_order), deleted_at=VALUES(deleted_at), show_in_lib=VALUES(show_in_lib)"
          else
            "INSERT INTO sync_categories (user_id, category_id, created_at, sort_key, track, title, sort_order, deleted_at, show_in_lib) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(user_id, category_id) DO UPDATE SET created_at=excluded.created_at, sort_key=excluded.sort_key, track=excluded.track, title=excluded.title, sort_order=excluded.sort_order, deleted_at=excluded.deleted_at, show_in_lib=excluded.show_in_lib"
          end
    conn.exec(sql, user, row.id, row.created_at, row.sort_key, row.track, truncate(row.title, 120), row.order, row.deleted_at, row.show_in_lib)
    clocks[row.id] = {row.created_at, row.deleted_at}
  end

  private def self.save_favourite(conn : DB::Connection, dialect : String, user : Int64, row : Favourite,
                                  clocks : Hash(Tuple(Int64, Int32), Tuple(Int64, Int64))) : Nil
    key = {row.manga_id, row.category_id}
    old = clocks[key]?
    return if old && !accept_library(old[0], old[1], row.created_at, row.deleted_at)
    sql = if dialect == "mysql"
            "INSERT INTO sync_favourites (user_id, manga_id, category_id, sort_key, pinned, created_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE sort_key=VALUES(sort_key), pinned=VALUES(pinned), created_at=VALUES(created_at), deleted_at=VALUES(deleted_at)"
          else
            "INSERT INTO sync_favourites (user_id, manga_id, category_id, sort_key, pinned, created_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(user_id, manga_id, category_id) DO UPDATE SET sort_key=excluded.sort_key, pinned=excluded.pinned, created_at=excluded.created_at, deleted_at=excluded.deleted_at"
          end
    conn.exec(sql, user, row.manga_id, row.category_id, row.sort_key, row.pinned, row.created_at, row.deleted_at)
    clocks[key] = {row.created_at, row.deleted_at}
  end

  private def self.read_history(conn : DB::Connection, user : Int64) : {Array(History), Int64}
    rows = [] of History
    conn.query("SELECT h.manga_id, m.payload, h.created_at, h.updated_at, h.chapter_id, h.page, h.scroll, h.percent, h.chapters, h.deleted_at FROM sync_history h JOIN sync_manga m ON m.id = h.manga_id WHERE h.user_id = ? ORDER BY h.manga_id", user) do |rs|
      rs.each do
        rows << History.new(rs.read(Int64), Manga.from_json(rs.read(String)), rs.read(Int64), rs.read(Int64),
          rs.read(Int64), rs.read(Int32), rs.read(Float32), rs.read(Float32), rs.read(Int32), rs.read(Int64))
      end
    end
    hydrate_tags(conn, rows.map(&.manga))
    {rows, timestamp(conn, user, true)}
  end

  private def self.read_favourites(conn : DB::Connection, user : Int64) : {Array(Category), Array(Favourite), Int64}
    categories = [] of Category
    conn.query("SELECT category_id, created_at, sort_key, track, title, sort_order, deleted_at, show_in_lib FROM sync_categories WHERE user_id = ? ORDER BY category_id", user) do |rs|
      rs.each do
        categories << Category.new(rs.read(Int32), rs.read(Int64), rs.read(Int32), rs.read(Bool),
          rs.read(String), rs.read(String), rs.read(Int64), rs.read(Bool))
      end
    end
    favourites = [] of Favourite
    conn.query("SELECT f.manga_id, m.payload, f.category_id, f.sort_key, f.pinned, f.created_at, f.deleted_at FROM sync_favourites f JOIN sync_manga m ON m.id = f.manga_id WHERE f.user_id = ? ORDER BY f.manga_id, f.category_id", user) do |rs|
      rs.each do
        favourites << Favourite.new(rs.read(Int64), Manga.from_json(rs.read(String)), rs.read(Int32),
          rs.read(Int32), rs.read(Bool), rs.read(Int64), rs.read(Int64))
      end
    end
    hydrate_tags(conn, favourites.map(&.manga))
    {categories, favourites, timestamp(conn, user, false)}
  end

  def self.history(db : DB::Database, dialect : String, user : Int64, input : HistoryInput?) : {Array(History), Int64}
    db.transaction do |transaction|
      conn = transaction.connection
      if input
        lock_user(conn, dialect, user)
        clocks = nil
        input.history.each do |row|
          save_manga(conn, dialect, row.manga)
          # The first clock read originally followed the first card write; retain that snapshot boundary.
          current_clocks = clocks || load_history_clocks(conn, user, input.history)
          clocks = current_clocks
          save_history(conn, dialect, user, row, current_clocks)
        end
      end
      result = read_history(conn, user)
      set_timestamp(conn, user, true) if input
      result
    end.not_nil!
  end

  def self.favourites(db : DB::Database, dialect : String, user : Int64, input : FavouritesInput?) : {Array(Category), Array(Favourite), Int64}
    db.transaction do |transaction|
      conn = transaction.connection
      if input
        lock_user(conn, dialect, user)
        unless input.categories.empty? && input.favourites.empty?
          category_clocks = load_category_clocks(conn, user, input)
          input.categories.each { |row| save_category(conn, dialect, user, row, category_clocks) }
          favourite_clocks = nil
          input.favourites.each do |row|
            raise InvalidPackage.new("Favourite references a missing category") unless category_clocks.has_key?(row.category_id)
            save_manga(conn, dialect, row.manga)
            current_clocks = favourite_clocks || load_favourite_clocks(conn, user, input.favourites)
            favourite_clocks = current_clocks
            save_favourite(conn, dialect, user, row, current_clocks)
          end
        end
      end
      result = read_favourites(conn, user)
      set_timestamp(conn, user, false) if input
      result
    end.not_nil!
  end

  def self.catalog(db : DB::Database, offset : Int32, limit : Int32) : Array(Manga)
    db.transaction do |transaction|
      conn = transaction.connection
      rows = conn.query_all("SELECT payload FROM sync_manga ORDER BY id LIMIT ? OFFSET ?", limit, offset, as: String).map { |payload| Manga.from_json(payload) }
      hydrate_tags(conn, rows)
      rows
    end.not_nil!
  end

  def self.catalog_item(db : DB::Database, id : Int64) : Manga?
    db.transaction do |transaction|
      conn = transaction.connection
      payload = conn.query_one?("SELECT payload FROM sync_manga WHERE id = ?", id, as: String)
      if payload
        manga = Manga.from_json(payload)
        hydrate_tags(conn, [manga])
        manga
      end
    end
  end
end
