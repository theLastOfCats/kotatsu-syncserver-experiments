using System;
using System.Collections.Generic;
using System.Linq;
using System.Data;
using System.Data.Common;
using Microsoft.Data.Sqlite;

internal static class SyncStorage
{
    private const string MangaColumns = "m.id,m.title,m.alt_title,m.url,m.public_url,m.rating,m.content_rating,m.cover_url,m.large_cover_url,m.state,m.author,m.source,m.nsfw";

    public static void Initialize()
    {
        using var connection = Database.Open();
        string engine = Database.Dialect == "mysql" ? " ENGINE=InnoDB" : "";
        string[] tables =
        [
            "CREATE TABLE IF NOT EXISTS sync_manga (id BIGINT NOT NULL PRIMARY KEY,title TEXT NOT NULL,alt_title TEXT NULL,url TEXT NOT NULL,public_url TEXT NOT NULL,rating DOUBLE NOT NULL,content_rating TEXT NULL,cover_url TEXT NOT NULL,large_cover_url TEXT NOT NULL,state TEXT NULL,author TEXT NULL,source TEXT NOT NULL,nsfw BOOLEAN NOT NULL)",
            "CREATE TABLE IF NOT EXISTS sync_tags (id BIGINT NOT NULL PRIMARY KEY,title TEXT NOT NULL,tag_key TEXT NOT NULL,source TEXT NOT NULL,pinned BOOLEAN NOT NULL)",
            "CREATE TABLE IF NOT EXISTS sync_manga_tags (manga_id BIGINT NOT NULL,tag_id BIGINT NOT NULL,PRIMARY KEY(manga_id,tag_id),FOREIGN KEY(manga_id) REFERENCES sync_manga(id),FOREIGN KEY(tag_id) REFERENCES sync_tags(id))",
            "CREATE TABLE IF NOT EXISTS sync_categories (user_id BIGINT NOT NULL,category_id INT NOT NULL,created_at BIGINT NOT NULL,sort_key INT NOT NULL,track BOOLEAN NOT NULL,title TEXT NOT NULL,sort_order LONGTEXT NOT NULL,deleted_at BIGINT NOT NULL,show_in_lib BOOLEAN NOT NULL,PRIMARY KEY(user_id,category_id),FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE)",
            "CREATE TABLE IF NOT EXISTS sync_history (user_id BIGINT NOT NULL,manga_id BIGINT NOT NULL,created_at BIGINT NOT NULL,updated_at BIGINT NOT NULL,chapter_id BIGINT NOT NULL,page INT NOT NULL,scroll DOUBLE NOT NULL,percent DOUBLE NOT NULL,chapters INT NOT NULL,deleted_at BIGINT NOT NULL,PRIMARY KEY(user_id,manga_id),FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,FOREIGN KEY(manga_id) REFERENCES sync_manga(id))",
            "CREATE TABLE IF NOT EXISTS sync_favourites (user_id BIGINT NOT NULL,manga_id BIGINT NOT NULL,category_id INT NOT NULL,sort_key INT NOT NULL,pinned BOOLEAN NOT NULL,created_at BIGINT NOT NULL,deleted_at BIGINT NOT NULL,PRIMARY KEY(user_id,manga_id,category_id),FOREIGN KEY(user_id,category_id) REFERENCES sync_categories(user_id,category_id) ON DELETE CASCADE,FOREIGN KEY(manga_id) REFERENCES sync_manga(id))",
            "CREATE TABLE IF NOT EXISTS sync_state (user_id BIGINT NOT NULL PRIMARY KEY,history_timestamp BIGINT NOT NULL,favourites_timestamp BIGINT NOT NULL,FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE)"
        ];
        foreach (string table in tables)
        {
            using var command = connection.CreateCommand();
            command.CommandText = table + engine;
            command.ExecuteNonQuery();
        }
    }

    private static DbTransaction Begin(DbConnection connection, bool write) => connection is SqliteConnection sqlite
        ? sqlite.BeginTransaction(deferred: !write)
        : connection.BeginTransaction(write ? IsolationLevel.ReadCommitted : IsolationLevel.RepeatableRead);

    private static DbCommand Command(DbConnection connection, DbTransaction transaction, string sql)
    {
        var command = connection.CreateCommand();
        command.Transaction = transaction;
        command.CommandText = sql;
        return command;
    }

    private static void Parameter(DbCommand command, string name, object? value)
    {
        var parameter = command.CreateParameter();
        parameter.ParameterName = name;
        parameter.Value = value ?? DBNull.Value;
        command.Parameters.Add(parameter);
    }

    private static void Cell(DbCommand command, int row, int column, object? value) =>
        Parameter(command, $"@p{row}_{column}", value);

    // Keep each statement below SQLite's traditional 999-parameter limit.
    private static void Upsert<T>(DbConnection connection, DbTransaction transaction, string table,
        string[] columns, string[] keys, IReadOnlyList<T> rows, Action<DbCommand, int, T> bind)
    {
        int size = Math.Max(1, 900 / columns.Length);
        for (int start = 0; start < rows.Count; start += size)
        {
            int count = Math.Min(size, rows.Count - start);
            using var command = Command(connection, transaction, "");
            var sql = new System.Text.StringBuilder();
            sql.Append("INSERT INTO ").Append(table).Append(" (").AppendJoin(',', columns).Append(") VALUES ");
            for (int row = 0; row < count; row++)
            {
                if (row > 0) sql.Append(',');
                sql.Append('(');
                for (int col = 0; col < columns.Length; col++)
                {
                    if (col > 0) sql.Append(',');
                    sql.Append("@p").Append(row).Append('_').Append(col);
                }
                sql.Append(')');
                bind(command, row, rows[start + row]);
            }
            var updates = columns.Where(column => !keys.Contains(column)).ToArray();
            if (Database.Dialect == "sqlite")
            {
                sql.Append(" ON CONFLICT (").AppendJoin(',', keys).Append(')');
                if (updates.Length == 0) sql.Append(" DO NOTHING");
                else
                {
                    sql.Append(" DO UPDATE SET ");
                    sql.AppendJoin(',', updates.Select(column => $"{column}=excluded.{column}"));
                }
            }
            else
            {
                sql.Append(" ON DUPLICATE KEY UPDATE ");
                if (updates.Length == 0) sql.Append(keys[0]).Append('=').Append(keys[0]);
                else sql.AppendJoin(',', updates.Select(column => $"{column}=VALUES({column})"));
            }
            command.CommandText = sql.ToString();
            command.ExecuteNonQuery();
        }
    }

    // The user row serializes all writes for an account, including across server
    // processes. SQLite's immediate write transaction provides the same guarantee.
    private static void LockUser(DbConnection connection, DbTransaction transaction, long userId)
    {
        string suffix = Database.Dialect == "mysql" ? " FOR UPDATE" : "";
        using var command = Command(connection, transaction, "SELECT id FROM users WHERE id=@user" + suffix);
        Parameter(command, "@user", userId);
        if (command.ExecuteScalar() is null) throw new SyncAccountMissingException();
    }

    private static string Clip(string value, int length) => value.Length <= length ? value : value[..length];
    private static string? ClipNullable(string? value, int length) => value is null ? null : Clip(value, length);

    // Manga and tags are global in the public API, whereas library rows are
    // scoped to a user. Reference semantics retain previously linked tags.
    private static void SaveManga(DbConnection connection, DbTransaction transaction, IEnumerable<SyncManga> cards)
    {
        var manga = new Dictionary<long, SyncManga>();
        var tags = new Dictionary<long, SyncTag>();
        var links = new HashSet<(long MangaId, long TagId)>();
        foreach (var card in cards)
        {
            manga[card.Id] = card;
            foreach (var tag in card.Tags)
            {
                tags[tag.Id] = tag;
                links.Add((card.Id, tag.Id));
            }
        }

        // Across accounts acquire catalog locks in a common order, before
        // account-owned rows, to avoid lock-order inversions on MySQL.
        var orderedManga = manga.Values.OrderBy(x => x.Id).ToArray();
        Upsert(connection, transaction, "sync_manga",
            ["id", "title", "alt_title", "url", "public_url", "rating", "content_rating", "cover_url", "large_cover_url", "state", "author", "source", "nsfw"],
            ["id"], orderedManga, static (c, n, x) =>
            {
                Cell(c,n,0,x.Id); Cell(c,n,1,Clip(x.Title,100)); Cell(c,n,2,ClipNullable(x.AltTitle,100));
                Cell(c,n,3,Clip(x.Url,255)); Cell(c,n,4,Clip(x.PublicUrl,255)); Cell(c,n,5,(double)x.Rating);
                Cell(c,n,6,x.ContentRating is "SAFE" or "SUGGESTIVE" or "ADULT" ? x.ContentRating : null);
                Cell(c,n,7,Clip(x.CoverUrl,255)); Cell(c,n,8,ClipNullable(x.LargeCoverUrl,255) ?? "");
                Cell(c,n,9,x.State is "ONGOING" or "FINISHED" or "ABANDONED" or "PAUSED" or "UPCOMING" ? x.State : null);
                Cell(c,n,10,ClipNullable(x.Author,64)); Cell(c,n,11,Clip(x.Source,32)); Cell(c,n,12,x.Nsfw ? 1 : 0);
            });
        var orderedTags = tags.Values.OrderBy(x => x.Id).ToArray();
        Upsert(connection, transaction, "sync_tags", ["id", "title", "tag_key", "source", "pinned"], ["id"], orderedTags,
            static (c, n, x) =>
            {
                Cell(c,n,0,x.Id); Cell(c,n,1,Clip(x.Title,64)); Cell(c,n,2,Clip(x.Key,120));
                Cell(c,n,3,Clip(x.Source,32)); Cell(c,n,4,x.Pinned ? 1 : 0);
            });
        var orderedLinks = links.OrderBy(x => x.MangaId).ThenBy(x => x.TagId).ToArray();
        Upsert(connection, transaction, "sync_manga_tags", ["manga_id", "tag_id"], ["manga_id", "tag_id"], orderedLinks,
            static (c, n, x) => { Cell(c,n,0,x.MangaId); Cell(c,n,1,x.TagId); });
    }

    public static SyncHistoryPackage GetHistory(long userId)
    {
        using var connection = Database.Open();
        using var transaction = Begin(connection, false);
        var result = ReadHistory(connection, transaction, userId);
        transaction.Commit();
        return result;
    }

    public static SyncFavouritesPackage GetFavourites(long userId)
    {
        using var connection = Database.Open();
        using var transaction = Begin(connection, false);
        var result = ReadFavourites(connection, transaction, userId);
        transaction.Commit();
        return result;
    }

    public static SyncHistoryPackage PutHistory(long userId, SyncHistoryPackage package)
    {
        using var connection = Database.Open();
        using var transaction = Begin(connection, true);
        LockUser(connection, transaction, userId);
        SaveManga(connection, transaction, package.History.Select(x => x.Manga));

        var stored = new Dictionary<long, (long Updated, long Deleted, long Created, long Chapter, int Page, double Scroll, double Percent, int Chapters)>();
        using (var command = Command(connection, transaction, "SELECT manga_id,updated_at,deleted_at,created_at,chapter_id,page,scroll,percent,chapters FROM sync_history WHERE user_id=@user"))
        {
            Parameter(command, "@user", userId);
            using var reader = command.ExecuteReader();
            while (reader.Read())
                stored.Add(reader.GetInt64(0), (reader.GetInt64(1), reader.GetInt64(2), reader.GetInt64(3),
                    reader.GetInt64(4), reader.GetInt32(5), reader.GetDouble(6), reader.GetDouble(7), reader.GetInt32(8)));
        }
        // Validation guarantees one row per manga. Keep catalog writes and the
        // readback even when none of the account's history rows change.
        List<SyncHistory>? writes = null;
        foreach (var row in package.History)
        {
            var exists = stored.TryGetValue(row.MangaId, out var old);
            if (exists && !HistoryWins(row, old.Updated, old.Deleted)) continue;
            if (!exists || row.UpdatedAt != old.Updated || row.DeletedAt != old.Deleted ||
                row.CreatedAt != old.Created || row.ChapterId != old.Chapter || row.Page != old.Page ||
                BitConverter.DoubleToInt64Bits(row.Scroll) != BitConverter.DoubleToInt64Bits(old.Scroll) ||
                BitConverter.DoubleToInt64Bits(row.Percent) != BitConverter.DoubleToInt64Bits(old.Percent) ||
                row.Chapters != old.Chapters)
                (writes ??= []).Add(row);
        }
        if (writes is not null)
        {
            writes.Sort(static (x, y) => x.MangaId.CompareTo(y.MangaId));
            Upsert(connection, transaction, "sync_history",
                ["user_id", "manga_id", "created_at", "updated_at", "chapter_id", "page", "scroll", "percent", "chapters", "deleted_at"],
                ["user_id", "manga_id"], writes, (c, n, x) =>
                {
                    Cell(c,n,0,userId); Cell(c,n,1,x.MangaId); Cell(c,n,2,x.CreatedAt); Cell(c,n,3,x.UpdatedAt);
                    Cell(c,n,4,x.ChapterId); Cell(c,n,5,x.Page); Cell(c,n,6,(double)x.Scroll); Cell(c,n,7,(double)x.Percent);
                    Cell(c,n,8,x.Chapters); Cell(c,n,9,x.DeletedAt);
                });
        }
        var result = ReadHistory(connection, transaction, userId);
        SetTimestamp(connection, transaction, userId, history: true);
        transaction.Commit();
        return result;
    }

    private static bool HistoryWins(SyncHistory incoming, long updated, long deleted)
    {
        if (incoming.UpdatedAt < updated) return false;
        if (incoming.UpdatedAt > updated)
            return incoming.DeletedAt != 0 || deleted == 0 || incoming.UpdatedAt > deleted;
        if (deleted != 0 && incoming.DeletedAt == 0) return false;
        if (deleted != 0 && incoming.DeletedAt != 0 && incoming.DeletedAt < deleted) return false;
        return true;
    }

    public static SyncFavouritesPackage PutFavourites(long userId, SyncFavouritesPackage package)
    {
        using var connection = Database.Open();
        using var transaction = Begin(connection, true);
        LockUser(connection, transaction, userId);

        // A favourite must point at this user's existing or incoming category.
        // Check the whole packet before making any catalog or library writes.
        var categoryIds = new HashSet<int>(package.Categories.Select(x => x.Id));
        using (var command = Command(connection, transaction, "SELECT category_id FROM sync_categories WHERE user_id=@user"))
        {
            Parameter(command, "@user", userId);
            using var reader = command.ExecuteReader();
            while (reader.Read()) categoryIds.Add(reader.GetInt32(0));
        }
        if (package.Favourites.Any(x => !categoryIds.Contains(x.CategoryId))) throw new InvalidSyncPackageException();

        SaveManga(connection, transaction, package.Favourites.Select(x => x.Manga));
        var categories = package.Categories.OrderBy(x => x.Id).ToArray();
        Upsert(connection, transaction, "sync_categories",
            ["user_id", "category_id", "created_at", "sort_key", "track", "title", "sort_order", "deleted_at", "show_in_lib"],
            ["user_id", "category_id"], categories, (c, n, x) =>
            {
                Cell(c,n,0,userId); Cell(c,n,1,x.Id); Cell(c,n,2,x.CreatedAt); Cell(c,n,3,x.SortKey);
                Cell(c,n,4,x.Track ? 1 : 0); Cell(c,n,5,Clip(x.Title,120)); Cell(c,n,6,x.Order);
                Cell(c,n,7,x.DeletedAt); Cell(c,n,8,x.ShowInLib ? 1 : 0);
            });
        var favourites = package.Favourites.OrderBy(x => x.MangaId).ThenBy(x => x.CategoryId).ToArray();
        Upsert(connection, transaction, "sync_favourites",
            ["user_id", "manga_id", "category_id", "sort_key", "pinned", "created_at", "deleted_at"],
            ["user_id", "manga_id", "category_id"], favourites, (c, n, x) =>
            {
                Cell(c,n,0,userId); Cell(c,n,1,x.MangaId); Cell(c,n,2,x.CategoryId); Cell(c,n,3,x.SortKey);
                Cell(c,n,4,x.Pinned ? 1 : 0); Cell(c,n,5,x.CreatedAt); Cell(c,n,6,x.DeletedAt);
            });
        var result = ReadFavourites(connection, transaction, userId);
        SetTimestamp(connection, transaction, userId, history: false);
        transaction.Commit();
        return result;
    }

    private static void SetTimestamp(DbConnection connection, DbTransaction transaction, long userId, bool history)
    {
        string field = history ? "history_timestamp" : "favourites_timestamp";
        string value = Database.Dialect == "sqlite" ? "excluded." + field : "VALUES(" + field + ")";
        string conflict = Database.Dialect == "sqlite" ? " ON CONFLICT (user_id) DO UPDATE SET " : " ON DUPLICATE KEY UPDATE ";
        using var command = Command(connection, transaction,
            "INSERT INTO sync_state(user_id,history_timestamp,favourites_timestamp) VALUES(@user,@history,@favourites)" + conflict + field + "=" + value);
        long now = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        Parameter(command, "@user", userId);
        Parameter(command, "@history", history ? now : 0L);
        Parameter(command, "@favourites", history ? 0L : now);
        command.ExecuteNonQuery();
    }

    private static long Timestamp(DbConnection connection, DbTransaction transaction, long userId, bool history)
    {
        string field = history ? "history_timestamp" : "favourites_timestamp";
        using var command = Command(connection, transaction, "SELECT " + field + " FROM sync_state WHERE user_id=@user");
        Parameter(command, "@user", userId);
        object? result = command.ExecuteScalar();
        return result is null or DBNull ? 0 : Convert.ToInt64(result, System.Globalization.CultureInfo.InvariantCulture);
    }

    private static SyncHistoryPackage ReadHistory(DbConnection connection, DbTransaction transaction, long userId)
    {
        var rows = new List<SyncHistory>();
        var manga = new Dictionary<long, SyncManga>();
        using (var command = Command(connection, transaction,
            "SELECT h.manga_id,h.created_at,h.updated_at,h.chapter_id,h.page,h.scroll,h.percent,h.chapters,h.deleted_at," + MangaColumns +
            " FROM sync_history h JOIN sync_manga m ON m.id=h.manga_id WHERE h.user_id=@user ORDER BY h.manga_id"))
        {
            Parameter(command, "@user", userId);
            using var reader = command.ExecuteReader();
            while (reader.Read())
            {
                var card = ReadManga(reader, 9);
                manga.Add(card.Id, card);
                rows.Add(new SyncHistory
                {
                    MangaId = reader.GetInt64(0), CreatedAt = reader.GetInt64(1), UpdatedAt = reader.GetInt64(2),
                    ChapterId = reader.GetInt64(3), Page = reader.GetInt32(4), Scroll = (float)reader.GetDouble(5),
                    Percent = (float)reader.GetDouble(6), Chapters = reader.GetInt32(7), DeletedAt = reader.GetInt64(8), Manga = card
                });
            }
        }
        ReadTags(connection, transaction, manga,
            "SELECT mt.manga_id,t.id,t.title,t.tag_key,t.source,t.pinned FROM sync_history h JOIN sync_manga_tags mt ON mt.manga_id=h.manga_id JOIN sync_tags t ON t.id=mt.tag_id WHERE h.user_id=@user ORDER BY mt.manga_id,t.id",
            c => Parameter(c, "@user", userId));
        return new SyncHistoryPackage { History = rows, Timestamp = Timestamp(connection, transaction, userId, true) };
    }

    private static SyncFavouritesPackage ReadFavourites(DbConnection connection, DbTransaction transaction, long userId)
    {
        var categories = new List<SyncCategory>();
        using (var command = Command(connection, transaction,
            "SELECT category_id,created_at,sort_key,track,title,sort_order,deleted_at,show_in_lib FROM sync_categories WHERE user_id=@user ORDER BY category_id"))
        {
            Parameter(command, "@user", userId);
            using var reader = command.ExecuteReader();
            while (reader.Read()) categories.Add(new SyncCategory
            {
                Id = reader.GetInt32(0), CreatedAt = reader.GetInt64(1), SortKey = reader.GetInt32(2), Track = reader.GetBoolean(3),
                Title = reader.GetString(4), Order = reader.GetString(5), DeletedAt = reader.GetInt64(6), ShowInLib = reader.GetBoolean(7)
            });
        }
        var rows = new List<SyncFavourite>();
        var manga = new Dictionary<long, SyncManga>();
        using (var command = Command(connection, transaction,
            "SELECT f.manga_id,f.category_id,f.sort_key,f.pinned,f.created_at,f.deleted_at," + MangaColumns +
            " FROM sync_favourites f JOIN sync_manga m ON m.id=f.manga_id WHERE f.user_id=@user ORDER BY f.manga_id,f.category_id"))
        {
            Parameter(command, "@user", userId);
            using var reader = command.ExecuteReader();
            while (reader.Read())
            {
                long id = reader.GetInt64(0);
                if (!manga.TryGetValue(id, out var card))
                {
                    card = ReadManga(reader, 6);
                    manga.Add(id, card);
                }
                rows.Add(new SyncFavourite
                {
                    MangaId = id, CategoryId = reader.GetInt32(1), SortKey = reader.GetInt32(2), Pinned = reader.GetBoolean(3),
                    CreatedAt = reader.GetInt64(4), DeletedAt = reader.GetInt64(5), Manga = card
                });
            }
        }
        ReadTags(connection, transaction, manga,
            "SELECT mt.manga_id,t.id,t.title,t.tag_key,t.source,t.pinned FROM (SELECT DISTINCT manga_id FROM sync_favourites WHERE user_id=@user) ids JOIN sync_manga_tags mt ON mt.manga_id=ids.manga_id JOIN sync_tags t ON t.id=mt.tag_id ORDER BY mt.manga_id,t.id",
            c => Parameter(c, "@user", userId));
        return new SyncFavouritesPackage { Categories = categories, Favourites = rows, Timestamp = Timestamp(connection, transaction, userId, false) };
    }

    public static List<SyncManga> GetManga(int offset, int limit)
    {
        using var connection = Database.Open();
        using var transaction = Begin(connection, false);
        var cards = new List<SyncManga>();
        var manga = new Dictionary<long, SyncManga>();
        using (var command = Command(connection, transaction,
            "SELECT " + MangaColumns + " FROM sync_manga m ORDER BY m.id LIMIT @limit OFFSET @offset"))
        {
            Parameter(command, "@limit", limit);
            Parameter(command, "@offset", offset);
            using var reader = command.ExecuteReader();
            while (reader.Read())
            {
                var card = ReadManga(reader, 0);
                cards.Add(card);
                manga.Add(card.Id, card);
            }
        }
        ReadTags(connection, transaction, manga,
            "SELECT mt.manga_id,t.id,t.title,t.tag_key,t.source,t.pinned FROM (SELECT id FROM sync_manga ORDER BY id LIMIT @limit OFFSET @offset) ids JOIN sync_manga_tags mt ON mt.manga_id=ids.id JOIN sync_tags t ON t.id=mt.tag_id ORDER BY mt.manga_id,t.id",
            c => { Parameter(c, "@limit", limit); Parameter(c, "@offset", offset); });
        transaction.Commit();
        return cards;
    }

    public static SyncManga? GetManga(long id)
    {
        using var connection = Database.Open();
        using var transaction = Begin(connection, false);
        SyncManga? card = null;
        using (var command = Command(connection, transaction, "SELECT " + MangaColumns + " FROM sync_manga m WHERE m.id=@id"))
        {
            Parameter(command, "@id", id);
            using var reader = command.ExecuteReader();
            if (reader.Read()) card = ReadManga(reader, 0);
        }
        if (card is not null)
            ReadTags(connection, transaction, new Dictionary<long, SyncManga> { [id] = card },
                "SELECT mt.manga_id,t.id,t.title,t.tag_key,t.source,t.pinned FROM sync_manga_tags mt JOIN sync_tags t ON t.id=mt.tag_id WHERE mt.manga_id=@id ORDER BY t.id",
                c => Parameter(c, "@id", id));
        transaction.Commit();
        return card;
    }

    private static SyncManga ReadManga(DbDataReader reader, int i) => new()
    {
        Id = reader.GetInt64(i), Title = reader.GetString(i+1), AltTitle = reader.IsDBNull(i+2) ? null : reader.GetString(i+2),
        Url = reader.GetString(i+3), PublicUrl = reader.GetString(i+4), Rating = (float)reader.GetDouble(i+5),
        ContentRating = reader.IsDBNull(i+6) ? null : reader.GetString(i+6), CoverUrl = reader.GetString(i+7),
        LargeCoverUrl = reader.GetString(i+8), State = reader.IsDBNull(i+9) ? null : reader.GetString(i+9),
        Author = reader.IsDBNull(i+10) ? null : reader.GetString(i+10), Source = reader.GetString(i+11), Nsfw = reader.GetBoolean(i+12),
        Tags = []
    };

    private static void ReadTags(DbConnection connection, DbTransaction transaction, Dictionary<long, SyncManga> manga,
        string sql, Action<DbCommand> bind)
    {
        if (manga.Count == 0) return;
        using var command = Command(connection, transaction, sql);
        bind(command);
        using var reader = command.ExecuteReader();
        while (reader.Read())
        {
            long id = reader.GetInt64(0);
            if (!manga.TryGetValue(id, out var card)) continue;
            card.Tags.Add(new SyncTag
            {
                Id = reader.GetInt64(1), Title = reader.GetString(2), Key = reader.GetString(3),
                Source = reader.GetString(4), Pinned = reader.GetBoolean(5)
            });
        }
    }
}

internal sealed class SyncAccountMissingException : Exception { }
