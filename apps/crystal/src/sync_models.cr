require "json"

module CrystalSync
  class InvalidPackage < Exception
  end

  class MangaTag
    include JSON::Serializable

    @[JSON::Field(key: "tag_id")]
    property id : Int64
    property title : String
    property key : String
    property source : String
    property pinned : Bool = false

    def_equals_and_hash id, title, key, source, pinned
  end

  @[JSON::Serializable::Options(emit_nulls: true)]
  class Manga
    include JSON::Serializable

    @[JSON::Field(key: "manga_id")]
    property id : Int64
    property title : String
    @[JSON::Field(presence: true)]
    property alt_title : String?
    property url : String
    property public_url : String
    property rating : Float32
    property content_rating : String?
    property cover_url : String
    @[JSON::Field(presence: true)]
    property large_cover_url : String?
    property tags : Array(MangaTag)
    @[JSON::Field(presence: true)]
    property state : String?
    @[JSON::Field(presence: true)]
    property author : String?
    property source : String
    property nsfw : Bool = false

    @[JSON::Field(ignore: true)]
    @alt_title_present : Bool = false
    @[JSON::Field(ignore: true)]
    @large_cover_url_present : Bool = false
    @[JSON::Field(ignore: true)]
    @state_present : Bool = false
    @[JSON::Field(ignore: true)]
    @author_present : Bool = false

    def_equals id, title, alt_title, url, public_url, rating, content_rating, cover_url, large_cover_url, tags, state, author, source, nsfw

    protected def after_initialize
      unless @alt_title_present && @large_cover_url_present && @state_present && @author_present
        raise InvalidPackage.new("Missing nullable manga field")
      end
      # Tags are a set in the reference DTO, not an ordered list.
      @tags.sort_by!(&.id)
      @tags.uniq!
    end
  end

  class History
    include JSON::Serializable

    property manga_id : Int64
    property manga : Manga
    property created_at : Int64
    property updated_at : Int64
    property chapter_id : Int64
    property page : Int32
    property scroll : Float32
    property percent : Float32
    property chapters : Int32 = -1
    property deleted_at : Int64

    def initialize(@manga_id : Int64, @manga : Manga, @created_at : Int64, @updated_at : Int64,
                   @chapter_id : Int64, @page : Int32, @scroll : Float32, @percent : Float32,
                   @chapters : Int32, @deleted_at : Int64)
    end

    def_equals manga_id, manga, created_at, updated_at, chapter_id, page, scroll, percent, chapters, deleted_at

    protected def after_initialize
      raise InvalidPackage.new("Manga id mismatch") unless @manga_id == @manga.id
    end
  end

  class Category
    include JSON::Serializable

    @[JSON::Field(key: "category_id")]
    property id : Int32
    property created_at : Int64
    property sort_key : Int32
    property track : Bool
    property title : String
    property order : String
    property deleted_at : Int64
    property show_in_lib : Bool

    def initialize(@id : Int32, @created_at : Int64, @sort_key : Int32, @track : Bool,
                   @title : String, @order : String, @deleted_at : Int64, @show_in_lib : Bool)
    end

    def_equals id, created_at, sort_key, track, title, order, deleted_at, show_in_lib
  end

  class Favourite
    include JSON::Serializable

    property manga_id : Int64
    property manga : Manga
    property category_id : Int32
    property sort_key : Int32
    property pinned : Bool
    property created_at : Int64
    property deleted_at : Int64

    def initialize(@manga_id : Int64, @manga : Manga, @category_id : Int32, @sort_key : Int32,
                   @pinned : Bool, @created_at : Int64, @deleted_at : Int64)
    end

    def_equals manga_id, manga, category_id, sort_key, pinned, created_at, deleted_at

    protected def after_initialize
      raise InvalidPackage.new("Manga id mismatch") unless @manga_id == @manga.id
    end
  end

  class HistoryInput
    include JSON::Serializable

    getter history : Array(History)
    @[JSON::Field(presence: true)]
    getter timestamp : Int64?
    @[JSON::Field(ignore: true)]
    @timestamp_present : Bool = false

    protected def after_initialize
      raise InvalidPackage.new("Missing timestamp") unless @timestamp_present
    end
  end

  class FavouritesInput
    include JSON::Serializable

    getter categories : Array(Category)
    getter favourites : Array(Favourite)
    @[JSON::Field(presence: true)]
    getter timestamp : Int64?
    @[JSON::Field(ignore: true)]
    @timestamp_present : Bool = false

    protected def after_initialize
      raise InvalidPackage.new("Missing timestamp") unless @timestamp_present
    end
  end
end
