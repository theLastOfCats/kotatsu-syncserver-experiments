using System;
using System.Collections.Generic;
using System.Linq;
using System.Globalization;
using System.Text.Json;
using System.Text.Json.Serialization;

internal sealed record SyncTag
{
    public SyncTag() { }

    [JsonPropertyName("tag_id")] public required long Id { get; init; }
    [JsonPropertyName("title")] public required string Title { get; init; }
    [JsonPropertyName("key")] public required string Key { get; init; }
    [JsonPropertyName("source")] public required string Source { get; init; }
    [JsonPropertyName("pinned")] public bool Pinned { get; init; }
}

internal sealed record SyncManga
{
    public SyncManga() { }

    [JsonPropertyName("manga_id")] public required long Id { get; init; }
    [JsonPropertyName("title")] public required string Title { get; init; }
    [JsonPropertyName("alt_title")] public required string? AltTitle { get; init; }
    [JsonPropertyName("url")] public required string Url { get; init; }
    [JsonPropertyName("public_url")] public required string PublicUrl { get; init; }
    [JsonPropertyName("rating")] public required float Rating { get; init; }
    [JsonPropertyName("content_rating")] public string? ContentRating { get; init; }
    [JsonPropertyName("cover_url")] public required string CoverUrl { get; init; }
    [JsonPropertyName("large_cover_url")] public required string? LargeCoverUrl { get; init; }
    [JsonPropertyName("tags")] public required List<SyncTag> Tags { get; init; }
    [JsonPropertyName("state")] public required string? State { get; init; }
    [JsonPropertyName("author")] public required string? Author { get; init; }
    [JsonPropertyName("source")] public required string Source { get; init; }
    [JsonPropertyName("nsfw")] public bool Nsfw { get; init; }
}

internal sealed record SyncHistory
{
    public SyncHistory() { }

    [JsonPropertyName("manga_id")] public required long MangaId { get; init; }
    [JsonPropertyName("manga")] public required SyncManga Manga { get; init; }
    [JsonPropertyName("created_at")] public required long CreatedAt { get; init; }
    [JsonPropertyName("updated_at")] public required long UpdatedAt { get; init; }
    [JsonPropertyName("chapter_id")] public required long ChapterId { get; init; }
    [JsonPropertyName("page")] public required int Page { get; init; }
    [JsonPropertyName("scroll")] public required float Scroll { get; init; }
    [JsonPropertyName("percent")] public required float Percent { get; init; }
    [JsonPropertyName("chapters")] public int Chapters { get; init; } = -1;
    [JsonPropertyName("deleted_at")] public required long DeletedAt { get; init; }
}

internal sealed record SyncCategory
{
    public SyncCategory() { }

    [JsonPropertyName("category_id")] public required int Id { get; init; }
    [JsonPropertyName("created_at")] public required long CreatedAt { get; init; }
    [JsonPropertyName("sort_key")] public required int SortKey { get; init; }
    [JsonPropertyName("track")] public required bool Track { get; init; }
    [JsonPropertyName("title")] public required string Title { get; init; }
    [JsonPropertyName("order")] public required string Order { get; init; }
    [JsonPropertyName("deleted_at")] public required long DeletedAt { get; init; }
    [JsonPropertyName("show_in_lib")] public required bool ShowInLib { get; init; }
}

internal sealed record SyncFavourite
{
    public SyncFavourite() { }

    [JsonPropertyName("manga_id")] public required long MangaId { get; init; }
    [JsonPropertyName("manga")] public required SyncManga Manga { get; init; }
    [JsonPropertyName("category_id")] public required int CategoryId { get; init; }
    [JsonPropertyName("sort_key")] public required int SortKey { get; init; }
    [JsonPropertyName("pinned")] public required bool Pinned { get; init; }
    [JsonPropertyName("created_at")] public required long CreatedAt { get; init; }
    [JsonPropertyName("deleted_at")] public required long DeletedAt { get; init; }
}

internal sealed record SyncHistoryPackage
{
    public SyncHistoryPackage() { }

    [JsonPropertyName("history")] public required List<SyncHistory> History { get; init; }
    [JsonPropertyName("timestamp")] public required long? Timestamp { get; init; }
}

internal sealed record SyncFavouritesPackage
{
    public SyncFavouritesPackage() { }

    [JsonPropertyName("categories")] public required List<SyncCategory> Categories { get; init; }
    [JsonPropertyName("favourites")] public required List<SyncFavourite> Favourites { get; init; }
    [JsonPropertyName("timestamp")] public required long? Timestamp { get; init; }
}

// Kotlin serializes Float integers as 0.0/1.0, not as JSON integers. It also
// uses scientific notation below 0.001 and at or above 10000000.
internal sealed class SyncFloatConverter : JsonConverter<float>
{
    public override float Read(ref Utf8JsonReader reader, Type type, JsonSerializerOptions options)
    {
        if (reader.TokenType != JsonTokenType.Number || !reader.TryGetSingle(out float value) || !float.IsFinite(value))
            throw new JsonException("Invalid float");
        return value;
    }

    public override void Write(Utf8JsonWriter writer, float value, JsonSerializerOptions options)
    {
        if (!float.IsFinite(value)) throw new JsonException("Non-finite float");
        string raw = value.ToString("R", CultureInfo.InvariantCulture);
        bool negative = raw.StartsWith('-');
        if (negative) raw = raw[1..];
        int e = raw.IndexOfAny(['E', 'e']);
        int explicitExponent = e < 0 ? 0 : int.Parse(raw.AsSpan(e + 1), CultureInfo.InvariantCulture);
        string mantissa = e < 0 ? raw : raw[..e];
        int dot = mantissa.IndexOf('.');
        int decimalPosition = dot < 0 ? mantissa.Length : dot;
        string digits = mantissa.Replace(".", "", StringComparison.Ordinal);
        int first = 0;
        while (first < digits.Length && digits[first] == '0') first++;
        if (first == digits.Length)
        {
            writer.WriteRawValue(negative ? "-0.0" : "0.0");
            return;
        }
        int exponent = explicitExponent + decimalPosition - first - 1;
        digits = digits[first..].TrimEnd('0');
        string result;
        if (exponent < -3 || exponent >= 7)
        {
            string fraction = digits.Length == 1 ? "0" : digits[1..];
            result = digits[0] + "." + fraction + "E" + exponent.ToString(CultureInfo.InvariantCulture);
        }
        else
        {
            int position = exponent + 1;
            result = position <= 0
                ? "0." + new string('0', -position) + digits
                : position >= digits.Length
                    ? digits + new string('0', position - digits.Length) + ".0"
                    : digits[..position] + "." + digits[position..];
        }
        writer.WriteRawValue(negative ? "-" + result : result);
    }
}

internal sealed class InvalidSyncPackageException : Exception { }

[JsonSourceGenerationOptions(GenerationMode = JsonSourceGenerationMode.Metadata)]
[JsonSerializable(typeof(SyncHistoryPackage))]
[JsonSerializable(typeof(SyncFavouritesPackage))]
[JsonSerializable(typeof(SyncManga))]
[JsonSerializable(typeof(List<SyncManga>))]
internal partial class SyncJsonContext : JsonSerializerContext;

internal static class SyncModel
{
    public static readonly SyncJsonContext Json = CreateJsonContext();

    private static SyncJsonContext CreateJsonContext()
    {
        var options = new JsonSerializerOptions();
        options.Converters.Add(new SyncFloatConverter());
        return new SyncJsonContext(options);
    }

    public static void Validate(SyncHistoryPackage? package)
    {
        if (package?.History is null) throw new InvalidSyncPackageException();
        var ids = new HashSet<long>();
        foreach (var row in package.History)
        {
            if (row is null || !ids.Add(row.MangaId) || !float.IsFinite(row.Scroll) || !float.IsFinite(row.Percent))
                throw new InvalidSyncPackageException();
            ValidateManga(row.Manga, row.MangaId);
        }
    }

    public static void Validate(SyncFavouritesPackage? package)
    {
        if (package?.Categories is null || package.Favourites is null) throw new InvalidSyncPackageException();
        var categoryIds = new HashSet<int>();
        foreach (var category in package.Categories)
        {
            if (category is null || category.Title is null || category.Order is null || !categoryIds.Add(category.Id))
                throw new InvalidSyncPackageException();
        }
        var favouriteIds = new HashSet<(long, int)>();
        foreach (var favourite in package.Favourites)
        {
            if (favourite is null || !favouriteIds.Add((favourite.MangaId, favourite.CategoryId)))
                throw new InvalidSyncPackageException();
            ValidateManga(favourite.Manga, favourite.MangaId);
        }
    }

    private static void ValidateManga(SyncManga? manga, long id)
    {
        if (manga is null || manga.Id != id || manga.Title is null || manga.Url is null ||
            manga.PublicUrl is null || manga.CoverUrl is null || manga.Source is null ||
            manga.Tags is null || !float.IsFinite(manga.Rating))
            throw new InvalidSyncPackageException();
        var tags = new Dictionary<long, SyncTag>();
        foreach (var tag in manga.Tags)
        {
            if (tag is null || tag.Title is null || tag.Key is null || tag.Source is null ||
                (tags.TryGetValue(tag.Id, out var previous) && previous != tag))
                throw new InvalidSyncPackageException();
            tags[tag.Id] = tag;
        }
    }

    private static bool FloatEquals(float a, float b) =>
        BitConverter.SingleToInt32Bits(a) == BitConverter.SingleToInt32Bits(b);

    private static bool MangaEquals(SyncManga a, SyncManga b) =>
        a.Id == b.Id && a.Title == b.Title && a.AltTitle == b.AltTitle && a.Url == b.Url &&
        a.PublicUrl == b.PublicUrl && FloatEquals(a.Rating, b.Rating) && a.ContentRating == b.ContentRating &&
        a.CoverUrl == b.CoverUrl && a.LargeCoverUrl == b.LargeCoverUrl && a.State == b.State &&
        a.Author == b.Author && a.Source == b.Source && a.Nsfw == b.Nsfw &&
        a.Tags.ToHashSet().SetEquals(b.Tags);

    public static bool ContentEquals(SyncHistoryPackage a, SyncHistoryPackage b)
    {
        if (a.History.Count != b.History.Count) return false;
        for (int i = 0; i < a.History.Count; i++)
        {
            var x = a.History[i];
            var y = b.History[i];
            if (x.MangaId != y.MangaId || x.CreatedAt != y.CreatedAt || x.UpdatedAt != y.UpdatedAt ||
                x.ChapterId != y.ChapterId || x.Page != y.Page || !FloatEquals(x.Scroll, y.Scroll) ||
                !FloatEquals(x.Percent, y.Percent) || x.Chapters != y.Chapters || x.DeletedAt != y.DeletedAt ||
                !MangaEquals(x.Manga, y.Manga)) return false;
        }
        return true;
    }

    public static bool ContentEquals(SyncFavouritesPackage a, SyncFavouritesPackage b)
    {
        if (!a.Categories.SequenceEqual(b.Categories) || a.Favourites.Count != b.Favourites.Count) return false;
        for (int i = 0; i < a.Favourites.Count; i++)
        {
            var x = a.Favourites[i];
            var y = b.Favourites[i];
            if (x.MangaId != y.MangaId || x.CategoryId != y.CategoryId || x.SortKey != y.SortKey ||
                x.Pinned != y.Pinned || x.CreatedAt != y.CreatedAt || x.DeletedAt != y.DeletedAt ||
                !MangaEquals(x.Manga, y.Manga)) return false;
        }
        return true;
    }
}
