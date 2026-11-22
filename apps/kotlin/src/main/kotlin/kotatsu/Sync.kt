package kotatsu

import io.ktor.http.HttpStatusCode
import io.ktor.server.request.receiveText
import io.ktor.server.response.respond
import io.ktor.server.routing.Route
import io.ktor.server.routing.get
import io.ktor.server.routing.post
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import java.sql.Connection

private val syncJson = Json {
    ignoreUnknownKeys = true
    explicitNulls = true
    encodeDefaults = true
}

@Serializable
private data class SyncTag(
    @SerialName("tag_id") val id: Long,
    val title: String,
    val key: String,
    val source: String,
    val pinned: Boolean,
)

@Serializable
private data class SyncManga(
    @SerialName("manga_id") val id: Long,
    val title: String,
    @SerialName("alt_title") val altTitle: String?,
    val url: String,
    @SerialName("public_url") val publicUrl: String,
    val rating: Float,
    @SerialName("content_rating") val contentRating: String?,
    @SerialName("cover_url") val coverUrl: String,
    @SerialName("large_cover_url") val largeCoverUrl: String?,
    val tags: List<SyncTag>,
    val state: String?,
    val author: String?,
    val source: String,
    val nsfw: Boolean,
)

@Serializable
private data class SyncCategory(
    @SerialName("category_id") val id: Int,
    @SerialName("created_at") val createdAt: Long,
    @SerialName("sort_key") val sortKey: Int,
    val track: Boolean,
    val title: String,
    val order: String,
    @SerialName("deleted_at") val deletedAt: Long,
    @SerialName("show_in_lib") val showInLib: Boolean,
)

@Serializable
private data class SyncFavourite(
    @SerialName("manga_id") val mangaId: Long,
    val manga: SyncManga,
    @SerialName("category_id") val categoryId: Int,
    @SerialName("sort_key") val sortKey: Int,
    val pinned: Boolean,
    @SerialName("created_at") val createdAt: Long,
    @SerialName("deleted_at") val deletedAt: Long,
)

@Serializable
private data class SyncHistory(
    @SerialName("manga_id") val mangaId: Long,
    val manga: SyncManga,
    @SerialName("created_at") val createdAt: Long,
    @SerialName("updated_at") val updatedAt: Long,
    @SerialName("chapter_id") val chapterId: Long,
    val page: Int,
    val scroll: Float,
    val percent: Float,
    val chapters: Int,
    @SerialName("deleted_at") val deletedAt: Long,
)

@Serializable
private data class FavouriteData(
    val sortKey: Int,
    val pinned: Boolean,
    val createdAt: Long,
    val deletedAt: Long,
)

@Serializable
private data class HistoryData(
    val createdAt: Long,
    val updatedAt: Long,
    val chapterId: Long,
    val page: Int,
    val scroll: Float,
    val percent: Float,
    val chapters: Int,
    val deletedAt: Long,
)

private fun SyncFavourite.data() = FavouriteData(sortKey, pinned, createdAt, deletedAt)
private fun SyncHistory.data() = HistoryData(createdAt, updatedAt, chapterId, page, scroll, percent, chapters, deletedAt)

@Serializable
private data class FavouritesPacket(
    val categories: List<SyncCategory>,
    val favourites: List<SyncFavourite>,
    val timestamp: Long?,
)

@Serializable
private data class HistoryPacket(
    val history: List<SyncHistory>,
    val timestamp: Long?,
)

private class InvalidSync : IllegalArgumentException()

private fun JsonElement.objectValue(): JsonObject = this as? JsonObject ?: throw InvalidSync()
private fun JsonObject.required(name: String): JsonElement = this[name] ?: throw InvalidSync()
private fun JsonObject.array(name: String): JsonArray = required(name) as? JsonArray ?: throw InvalidSync()

private fun JsonElement.stringValue(): String {
    val primitive = this as? JsonPrimitive ?: throw InvalidSync()
    if (!primitive.isString) throw InvalidSync()
    return primitive.content
}

private fun JsonObject.string(name: String): String = required(name).stringValue()
private fun JsonObject.nullableString(name: String): String? = when (val value = required(name)) {
    JsonNull -> null
    else -> value.stringValue()
}

private fun JsonElement.longValue(): Long {
    val primitive = this as? JsonPrimitive ?: throw InvalidSync()
    if (primitive.isString) throw InvalidSync()
    return primitive.content.toLongOrNull() ?: throw InvalidSync()
}

private fun JsonObject.long(name: String): Long = required(name).longValue()
private fun JsonObject.int(name: String): Int {
    val value = long(name)
    if (value !in Int.MIN_VALUE.toLong()..Int.MAX_VALUE.toLong()) throw InvalidSync()
    return value.toInt()
}

private fun JsonObject.float(name: String): Float {
    val primitive = required(name) as? JsonPrimitive ?: throw InvalidSync()
    if (primitive.isString) throw InvalidSync()
    val value = primitive.content.toFloatOrNull() ?: throw InvalidSync()
    if (!value.isFinite()) throw InvalidSync()
    return value
}

private fun JsonElement.booleanValue(): Boolean {
    val primitive = this as? JsonPrimitive ?: throw InvalidSync()
    if (primitive.isString) throw InvalidSync()
    return when (primitive.content) {
        "true" -> true
        "false" -> false
        else -> throw InvalidSync()
    }
}

private fun JsonObject.boolean(name: String): Boolean = required(name).booleanValue()
private fun JsonObject.timestamp(): Long? = when (val value = required("timestamp")) {
    JsonNull -> null
    else -> value.longValue()
}

private fun parseTag(element: JsonElement): SyncTag {
    val obj = element.objectValue()
    return SyncTag(
        id = obj.long("tag_id"),
        title = obj.string("title"),
        key = obj.string("key"),
        source = obj.string("source"),
        pinned = obj["pinned"]?.booleanValue() ?: false,
    )
}

private fun parseManga(element: JsonElement): SyncManga {
    val obj = element.objectValue()
    return SyncManga(
        id = obj.long("manga_id"),
        title = obj.string("title"),
        altTitle = obj.nullableString("alt_title"),
        url = obj.string("url"),
        publicUrl = obj.string("public_url"),
        rating = obj.float("rating"),
        contentRating = if ("content_rating" in obj) obj.nullableString("content_rating") else null,
        coverUrl = obj.string("cover_url"),
        largeCoverUrl = obj.nullableString("large_cover_url"),
        tags = obj.array("tags").map(::parseTag).distinct(),
        state = obj.nullableString("state"),
        author = obj.nullableString("author"),
        source = obj.string("source"),
        nsfw = obj["nsfw"]?.booleanValue() ?: false,
    )
}

private fun parseCategory(element: JsonElement): SyncCategory {
    val obj = element.objectValue()
    return SyncCategory(
        id = obj.int("category_id"),
        createdAt = obj.long("created_at"),
        sortKey = obj.int("sort_key"),
        track = obj.boolean("track"),
        title = obj.string("title"),
        order = obj.string("order"),
        deletedAt = obj.long("deleted_at"),
        showInLib = obj.boolean("show_in_lib"),
    )
}

private fun parseFavourite(element: JsonElement): SyncFavourite {
    val obj = element.objectValue()
    val id = obj.long("manga_id")
    val manga = parseManga(obj.required("manga"))
    if (id != manga.id) throw InvalidSync()
    return SyncFavourite(
        mangaId = id,
        manga = manga,
        categoryId = obj.int("category_id"),
        sortKey = obj.int("sort_key"),
        pinned = obj.boolean("pinned"),
        createdAt = obj.long("created_at"),
        deletedAt = obj.long("deleted_at"),
    )
}

private fun parseHistory(element: JsonElement): SyncHistory {
    val obj = element.objectValue()
    val id = obj.long("manga_id")
    val manga = parseManga(obj.required("manga"))
    if (id != manga.id) throw InvalidSync()
    return SyncHistory(
        mangaId = id,
        manga = manga,
        createdAt = obj.long("created_at"),
        updatedAt = obj.long("updated_at"),
        chapterId = obj.long("chapter_id"),
        page = obj.int("page"),
        scroll = obj.float("scroll"),
        percent = obj.float("percent"),
        chapters = if ("chapters" in obj) obj.int("chapters") else -1,
        deletedAt = obj.long("deleted_at"),
    )
}

private fun parseFavouritesPacket(text: String): FavouritesPacket {
    val obj = syncJson.parseToJsonElement(text).objectValue()
    val categories = obj.array("categories").map(::parseCategory)
    val favourites = obj.array("favourites").map(::parseFavourite)
    if (categories.map { it.id }.toSet().size != categories.size ||
        favourites.map { it.mangaId to it.categoryId }.toSet().size != favourites.size
    ) throw InvalidSync()
    return FavouritesPacket(categories, favourites, obj.timestamp())
}

private fun parseHistoryPacket(text: String): HistoryPacket {
    val obj = syncJson.parseToJsonElement(text).objectValue()
    val history = obj.array("history").map(::parseHistory)
    if (history.map { it.mangaId }.toSet().size != history.size) throw InvalidSync()
    return HistoryPacket(history, obj.timestamp())
}

private val contentRatings = setOf("SAFE", "SUGGESTIVE", "ADULT")
private val mangaStates = setOf("ONGOING", "FINISHED", "ABANDONED", "PAUSED", "UPCOMING")

// The reference stores these fields with bounded lengths and converts an absent
// large cover to an empty string when reading its entity back from SQL.
private fun SyncManga.persisted(): SyncManga = copy(
    title = title.take(100),
    altTitle = altTitle?.take(100),
    url = url.take(255),
    publicUrl = publicUrl.take(255),
    contentRating = contentRating?.takeIf { it in contentRatings },
    coverUrl = coverUrl.take(255),
    largeCoverUrl = largeCoverUrl?.take(255) ?: "",
    tags = tags.map {
        it.copy(title = it.title.take(64), key = it.key.take(120), source = it.source.take(32))
    }.associateBy { it.id }.values.sortedBy { it.id },
    state = state?.takeIf { it in mangaStates },
    author = author?.take(64),
    source = source.take(32),
)

/** Creates the account-owned sync tables after the auth schema has been initialized. */
fun initSync(db: Database) {
    val mysql = db.dialect.equals("mysql", ignoreCase = true)
    val text = if (mysql) "LONGTEXT" else "TEXT"
    val engine = if (mysql) " ENGINE=InnoDB" else ""
    val catalogIndex = if (mysql) ", INDEX sync_manga_catalog (manga_id, user_id)" else ""
    db.transaction { connection ->
        connection.createStatement().use { statement ->
            statement.execute(
                """CREATE TABLE IF NOT EXISTS sync_manga (
                    user_id BIGINT NOT NULL,
                    manga_id BIGINT NOT NULL,
                    data $text NOT NULL,
                    PRIMARY KEY (user_id, manga_id),
                    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
                    $catalogIndex
                )$engine""".trimIndent(),
            )
            if (!mysql) {
                statement.execute("CREATE INDEX IF NOT EXISTS sync_manga_catalog ON sync_manga (manga_id, user_id)")
            }
            statement.execute(
                """CREATE TABLE IF NOT EXISTS sync_tags (
                    user_id BIGINT NOT NULL,
                    tag_id BIGINT NOT NULL,
                    data $text NOT NULL,
                    PRIMARY KEY (user_id, tag_id),
                    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
                )$engine""".trimIndent(),
            )
            statement.execute(
                """CREATE TABLE IF NOT EXISTS sync_manga_tags (
                    user_id BIGINT NOT NULL,
                    manga_id BIGINT NOT NULL,
                    tag_id BIGINT NOT NULL,
                    PRIMARY KEY (user_id, manga_id, tag_id),
                    FOREIGN KEY (user_id, manga_id) REFERENCES sync_manga(user_id, manga_id) ON DELETE CASCADE,
                    FOREIGN KEY (user_id, tag_id) REFERENCES sync_tags(user_id, tag_id) ON DELETE CASCADE
                )$engine""".trimIndent(),
            )
            statement.execute(
                """CREATE TABLE IF NOT EXISTS sync_categories (
                    user_id BIGINT NOT NULL,
                    category_id BIGINT NOT NULL,
                    data $text NOT NULL,
                    PRIMARY KEY (user_id, category_id),
                    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
                )$engine""".trimIndent(),
            )
            statement.execute(
                """CREATE TABLE IF NOT EXISTS sync_favourites (
                    user_id BIGINT NOT NULL,
                    category_id BIGINT NOT NULL,
                    manga_id BIGINT NOT NULL,
                    data $text NOT NULL,
                    PRIMARY KEY (user_id, category_id, manga_id),
                    FOREIGN KEY (user_id, category_id) REFERENCES sync_categories(user_id, category_id) ON DELETE CASCADE,
                    FOREIGN KEY (user_id, manga_id) REFERENCES sync_manga(user_id, manga_id) ON DELETE CASCADE
                )$engine""".trimIndent(),
            )
            statement.execute(
                """CREATE TABLE IF NOT EXISTS sync_history (
                    user_id BIGINT NOT NULL,
                    manga_id BIGINT NOT NULL,
                    data $text NOT NULL,
                    PRIMARY KEY (user_id, manga_id),
                    FOREIGN KEY (user_id, manga_id) REFERENCES sync_manga(user_id, manga_id) ON DELETE CASCADE
                )$engine""".trimIndent(),
            )
            statement.execute(
                """CREATE TABLE IF NOT EXISTS sync_timestamps (
                    user_id BIGINT NOT NULL PRIMARY KEY,
                    favourites_at BIGINT NOT NULL,
                    history_at BIGINT NOT NULL,
                    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
                )$engine""".trimIndent(),
            )
        }
    }
}

private data class SyncTimes(val favourites: Long, val history: Long)

private fun Connection.syncTimes(userId: Long): SyncTimes? =
    prepareStatement("SELECT favourites_at, history_at FROM sync_timestamps WHERE user_id = ?").use { statement ->
        statement.setLong(1, userId)
        statement.executeQuery().use { rows ->
            if (rows.next()) SyncTimes(rows.getLong(1), rows.getLong(2)) else null
        }
    }

private fun Connection.setSyncTime(userId: Long, current: SyncTimes?, favourites: Boolean) {
    val now = System.currentTimeMillis()
    if (current == null) {
        prepareStatement("INSERT INTO sync_timestamps (user_id, favourites_at, history_at) VALUES (?, ?, ?)").use {
            it.setLong(1, userId)
            it.setLong(2, if (favourites) now else 0)
            it.setLong(3, if (favourites) 0 else now)
            it.executeUpdate()
        }
    } else {
        val column = if (favourites) "favourites_at" else "history_at"
        prepareStatement("UPDATE sync_timestamps SET $column = ? WHERE user_id = ?").use {
            it.setLong(1, now)
            it.setLong(2, userId)
            it.executeUpdate()
        }
    }
}

private fun Connection.categories(userId: Long): List<SyncCategory> =
    prepareStatement("SELECT data FROM sync_categories WHERE user_id = ? ORDER BY category_id").use { statement ->
        statement.setLong(1, userId)
        statement.executeQuery().use { rows ->
            buildList { while (rows.next()) add(syncJson.decodeFromString<SyncCategory>(rows.getString(1))) }
        }
    }

private class MangaTags(
    val values: MutableMap<Long, SyncTag> = mutableMapOf(),
    val links: MutableMap<Long, java.util.SortedSet<Long>> = mutableMapOf(),
) {
    fun attach(manga: SyncManga): SyncManga = manga.copy(
        tags = links[manga.id]?.map { values.getValue(it) } ?: emptyList(),
    )
}

private fun Connection.mangaTags(userId: Long): MangaTags {
    val tags = MangaTags()
    prepareStatement(
        """SELECT mt.manga_id, t.data FROM sync_manga_tags mt
            JOIN sync_tags t ON t.user_id = mt.user_id AND t.tag_id = mt.tag_id
            WHERE mt.user_id = ? ORDER BY mt.manga_id, mt.tag_id""".trimIndent(),
    ).use { statement ->
        statement.setLong(1, userId)
        statement.executeQuery().use { rows ->
            while (rows.next()) {
                val tag = syncJson.decodeFromString<SyncTag>(rows.getString(2))
                tags.values[tag.id] = tag
                tags.links.getOrPut(rows.getLong(1)) { java.util.TreeSet() }.add(tag.id)
            }
        }
    }
    return tags
}

private fun Connection.favourites(userId: Long, tags: MangaTags = mangaTags(userId)): List<SyncFavourite> {
    return prepareStatement(
        """SELECT f.manga_id, f.category_id, f.data, m.data FROM sync_favourites f
            JOIN sync_manga m ON m.user_id = f.user_id AND m.manga_id = f.manga_id
            WHERE f.user_id = ? ORDER BY f.manga_id, f.category_id""".trimIndent(),
    ).use { statement ->
        statement.setLong(1, userId)
        statement.executeQuery().use { rows ->
            buildList {
                while (rows.next()) {
                    val data = syncJson.decodeFromString<FavouriteData>(rows.getString(3))
                    add(SyncFavourite(
                        mangaId = rows.getLong(1),
                        manga = tags.attach(syncJson.decodeFromString(rows.getString(4))),
                        categoryId = rows.getInt(2),
                        sortKey = data.sortKey,
                        pinned = data.pinned,
                        createdAt = data.createdAt,
                        deletedAt = data.deletedAt,
                    ))
                }
            }
        }
    }
}

private fun Connection.history(userId: Long, tags: MangaTags = mangaTags(userId)): List<SyncHistory> {
    return prepareStatement(
        """SELECT h.manga_id, h.data, m.data FROM sync_history h
            JOIN sync_manga m ON m.user_id = h.user_id AND m.manga_id = h.manga_id
            WHERE h.user_id = ? ORDER BY h.manga_id""".trimIndent(),
    ).use { statement ->
        statement.setLong(1, userId)
        statement.executeQuery().use { rows ->
            buildList {
                while (rows.next()) {
                    val data = syncJson.decodeFromString<HistoryData>(rows.getString(2))
                    add(SyncHistory(
                        mangaId = rows.getLong(1),
                        manga = tags.attach(syncJson.decodeFromString(rows.getString(3))),
                        createdAt = data.createdAt,
                        updatedAt = data.updatedAt,
                        chapterId = data.chapterId,
                        page = data.page,
                        scroll = data.scroll,
                        percent = data.percent,
                        chapters = data.chapters,
                        deletedAt = data.deletedAt,
                    ))
                }
            }
        }
    }
}

private class UserManga(val cards: MutableMap<Long, SyncManga>, val tags: MangaTags)

private fun Connection.mangaForUser(userId: Long, tags: MangaTags): UserManga {
    val cards = mutableMapOf<Long, SyncManga>()
    prepareStatement("SELECT manga_id, data FROM sync_manga WHERE user_id = ?").use { statement ->
        statement.setLong(1, userId)
        statement.executeQuery().use { rows ->
            while (rows.next()) cards[rows.getLong(1)] = syncJson.decodeFromString(rows.getString(2))
        }
    }
    return UserManga(cards, tags)
}

private fun Connection.putManga(userId: Long, manga: SyncManga, existing: UserManga) {
    val saved = manga.persisted()
    val core = saved.copy(tags = emptyList())
    val old = existing.cards[saved.id]
    if (old != core) {
        val sql = if (old == null) {
            "INSERT INTO sync_manga (data, user_id, manga_id) VALUES (?, ?, ?)"
        } else {
            "UPDATE sync_manga SET data = ? WHERE user_id = ? AND manga_id = ?"
        }
        prepareStatement(sql).use {
            it.setString(1, syncJson.encodeToString(core))
            it.setLong(2, userId)
            it.setLong(3, saved.id)
            it.executeUpdate()
        }
        existing.cards[saved.id] = core
    }
    for (tag in saved.tags) {
        val previous = existing.tags.values[tag.id]
        if (previous != tag) {
            val sql = if (previous == null) {
                "INSERT INTO sync_tags (data, user_id, tag_id) VALUES (?, ?, ?)"
            } else {
                "UPDATE sync_tags SET data = ? WHERE user_id = ? AND tag_id = ?"
            }
            prepareStatement(sql).use {
                it.setString(1, syncJson.encodeToString(tag))
                it.setLong(2, userId)
                it.setLong(3, tag.id)
                it.executeUpdate()
            }
            existing.tags.values[tag.id] = tag
        }
        val links = existing.tags.links.getOrPut(saved.id) { java.util.TreeSet() }
        if (links.add(tag.id)) {
            prepareStatement("INSERT INTO sync_manga_tags (user_id, manga_id, tag_id) VALUES (?, ?, ?)").use {
                it.setLong(1, userId)
                it.setLong(2, saved.id)
                it.setLong(3, tag.id)
                it.executeUpdate()
            }
        }
    }
}

private fun Connection.putCategory(userId: Long, category: SyncCategory, exists: Boolean) {
    val sql = if (exists) {
        "UPDATE sync_categories SET data = ? WHERE user_id = ? AND category_id = ?"
    } else {
        "INSERT INTO sync_categories (data, user_id, category_id) VALUES (?, ?, ?)"
    }
    prepareStatement(sql).use {
        it.setString(1, syncJson.encodeToString(category))
        it.setLong(2, userId)
        it.setLong(3, category.id.toLong())
        it.executeUpdate()
    }
}

private fun Connection.putFavourite(userId: Long, favourite: SyncFavourite, exists: Boolean) {
    val sql = if (exists) {
        "UPDATE sync_favourites SET data = ? WHERE user_id = ? AND category_id = ? AND manga_id = ?"
    } else {
        "INSERT INTO sync_favourites (data, user_id, category_id, manga_id) VALUES (?, ?, ?, ?)"
    }
    prepareStatement(sql).use {
        it.setString(1, syncJson.encodeToString(favourite.data()))
        it.setLong(2, userId)
        it.setLong(3, favourite.categoryId.toLong())
        it.setLong(4, favourite.mangaId)
        it.executeUpdate()
    }
}

private fun Connection.putHistory(userId: Long, history: SyncHistory, exists: Boolean) {
    val sql = if (exists) {
        "UPDATE sync_history SET data = ? WHERE user_id = ? AND manga_id = ?"
    } else {
        "INSERT INTO sync_history (data, user_id, manga_id) VALUES (?, ?, ?)"
    }
    prepareStatement(sql).use {
        it.setString(1, syncJson.encodeToString(history.data()))
        it.setLong(2, userId)
        it.setLong(3, history.mangaId)
        it.executeUpdate()
    }
}

// Created/deleted are the only version clocks on categories and favourites.
private fun favouriteWins(created: Long, deleted: Long, oldCreated: Long, oldDeleted: Long): Boolean {
    val incomingTime = maxOf(created, deleted)
    val oldTime = maxOf(oldCreated, oldDeleted)
    return incomingTime > oldTime ||
        (incomingTime == oldTime && (oldDeleted == 0L || deleted != 0L))
}

private fun historyWins(incoming: SyncHistory, old: SyncHistory): Boolean {
    if (incoming.updatedAt < old.updatedAt) return false
    if (incoming.updatedAt > old.updatedAt) {
        // A reading made before the removal is not a re-add, even if it was
        // newer than the old reading that the other phone removed.
        return old.deletedAt == 0L || incoming.deletedAt != 0L || incoming.updatedAt > old.deletedAt
    }
    if (old.deletedAt != 0L && incoming.deletedAt == 0L) return false
    if (old.deletedAt != 0L && incoming.deletedAt != 0L) return incoming.deletedAt >= old.deletedAt
    return true
}

private fun Connection.getFavourites(userId: Long): FavouritesPacket =
    FavouritesPacket(categories(userId), favourites(userId), syncTimes(userId)?.favourites ?: 0L)

private fun Connection.mergeFavourites(userId: Long, packet: FavouritesPacket): FavouritesPacket {
    val oldCategories = categories(userId).associateBy { it.id }
    val tags = mangaTags(userId)
    val oldFavourites = favourites(userId, tags).associateBy { it.mangaId to it.categoryId }
    val validCategories = oldCategories.keys + packet.categories.map { it.id }
    if (packet.favourites.any { it.categoryId !in validCategories }) throw InvalidSync()
    val manga = if (packet.favourites.isEmpty()) null else mangaForUser(userId, tags)
    val times = syncTimes(userId)

    for (input in packet.categories) {
        val row = input.copy(title = input.title.take(120))
        val old = oldCategories[row.id]
        if (old == null || favouriteWins(row.createdAt, row.deletedAt, old.createdAt, old.deletedAt)) {
            if (row != old) putCategory(userId, row, old != null)
        }
    }
    for (row in packet.favourites) {
        val old = oldFavourites[row.mangaId to row.categoryId]
        if (old == null || favouriteWins(row.createdAt, row.deletedAt, old.createdAt, old.deletedAt)) {
            putManga(userId, row.manga, checkNotNull(manga))
            if (old == null || row.data() != old.data()) putFavourite(userId, row, old != null)
        }
    }
    val result = FavouritesPacket(categories(userId), favourites(userId, tags), times?.favourites ?: 0L)
    setSyncTime(userId, times, favourites = true)
    return result
}

private fun Connection.getHistory(userId: Long): HistoryPacket =
    HistoryPacket(history(userId), syncTimes(userId)?.history ?: 0L)

private fun Connection.mergeHistory(userId: Long, packet: HistoryPacket): HistoryPacket {
    val tags = mangaTags(userId)
    val oldRows = history(userId, tags).associateBy { it.mangaId }
    val manga = if (packet.history.isEmpty()) null else mangaForUser(userId, tags)
    val times = syncTimes(userId)
    for (row in packet.history) {
        val old = oldRows[row.mangaId]
        if (old == null || historyWins(row, old)) {
            putManga(userId, row.manga, checkNotNull(manga))
            if (old == null || row.data() != old.data()) putHistory(userId, row, old != null)
        }
    }
    val result = HistoryPacket(history(userId, tags), times?.history ?: 0L)
    setSyncTime(userId, times, favourites = false)
    return result
}

private fun Connection.catalog(limit: Int, offset: Int): List<SyncManga> =
    prepareStatement(
        """SELECT page_rows.manga_id, page_rows.data, t.data FROM (
                SELECT m.user_id, m.manga_id, m.data FROM sync_manga m
                WHERE NOT EXISTS (
                    SELECT 1 FROM sync_manga earlier
                    WHERE earlier.manga_id = m.manga_id AND earlier.user_id < m.user_id
                )
                ORDER BY m.manga_id LIMIT ? OFFSET ?
            ) page_rows
            LEFT JOIN sync_manga_tags mt
                ON mt.user_id = page_rows.user_id AND mt.manga_id = page_rows.manga_id
            LEFT JOIN sync_tags t ON t.user_id = mt.user_id AND t.tag_id = mt.tag_id
            ORDER BY page_rows.manga_id, mt.tag_id""".trimIndent(),
    ).use { statement ->
        statement.setInt(1, limit)
        statement.setInt(2, offset)
        statement.executeQuery().use { rows ->
            buildList {
                var current: SyncManga? = null
                val tags = mutableListOf<SyncTag>()
                while (rows.next()) {
                    val id = rows.getLong(1)
                    if (current?.id != id) {
                        current?.let { add(it.copy(tags = tags.toList())) }
                        current = syncJson.decodeFromString(rows.getString(2))
                        tags.clear()
                    }
                    rows.getString(3)?.let { tags.add(syncJson.decodeFromString(it)) }
                }
                current?.let { add(it.copy(tags = tags.toList())) }
            }
        }
    }

private fun Connection.catalogManga(id: Long): SyncManga? =
    prepareStatement(
        """SELECT chosen.data, t.data FROM (
                SELECT user_id, manga_id, data FROM sync_manga
                WHERE manga_id = ? ORDER BY user_id LIMIT 1
            ) chosen
            LEFT JOIN sync_manga_tags mt ON mt.user_id = chosen.user_id AND mt.manga_id = chosen.manga_id
            LEFT JOIN sync_tags t ON t.user_id = mt.user_id AND t.tag_id = mt.tag_id
            ORDER BY mt.tag_id""".trimIndent(),
    ).use { statement ->
        statement.setLong(1, id)
        statement.executeQuery().use { rows ->
            if (rows.next()) {
                val manga = syncJson.decodeFromString<SyncManga>(rows.getString(1))
                val tags = mutableListOf<SyncTag>()
                do {
                    rows.getString(2)?.let { tags.add(syncJson.decodeFromString(it)) }
                } while (rows.next())
                manga.copy(tags = tags)
            } else {
                null
            }
        }
    }

fun Route.syncRoutes(db: Database, config: Config) {
    get("/resource/favourites") {
        val userId = call.authenticatedUserId(db, config) ?: return@get
        val result = withContext(Dispatchers.IO) { db.transaction(userId) { it.getFavourites(userId) } }
        call.respond(result)
    }
    post("/resource/favourites") {
        val userId = call.authenticatedUserId(db, config) ?: return@post
        val packet = try {
            parseFavouritesPacket(call.receiveText())
        } catch (_: IllegalArgumentException) {
            call.respond(HttpStatusCode.BadRequest)
            return@post
        }
        val result = try {
            withContext(Dispatchers.IO) { db.transaction(userId) { it.mergeFavourites(userId, packet) } }
        } catch (_: InvalidSync) {
            call.respond(HttpStatusCode.BadRequest)
            return@post
        }
        if (result.categories == packet.categories && result.favourites == packet.favourites) {
            call.respond(HttpStatusCode.NoContent)
        } else {
            call.respond(result)
        }
    }
    get("/resource/history") {
        val userId = call.authenticatedUserId(db, config) ?: return@get
        val result = withContext(Dispatchers.IO) { db.transaction(userId) { it.getHistory(userId) } }
        call.respond(result)
    }
    post("/resource/history") {
        val userId = call.authenticatedUserId(db, config) ?: return@post
        val packet = try {
            parseHistoryPacket(call.receiveText())
        } catch (_: IllegalArgumentException) {
            call.respond(HttpStatusCode.BadRequest)
            return@post
        }
        val result = withContext(Dispatchers.IO) { db.transaction(userId) { it.mergeHistory(userId, packet) } }
        if (result.history == packet.history) {
            call.respond(HttpStatusCode.NoContent)
        } else {
            call.respond(result)
        }
    }
    get("/manga") {
        val offset = call.request.queryParameters["offset"]?.toIntOrNull()
        val limit = call.request.queryParameters["limit"]?.toIntOrNull()
        if (offset == null || limit == null || offset < 0 || limit < 0) {
            call.respond(HttpStatusCode.BadRequest)
            return@get
        }
        val result = withContext(Dispatchers.IO) { db.transaction { it.catalog(limit, offset) } }
        call.respond(result)
    }
    get("/manga/{id}") {
        val id = call.parameters["id"]?.toLongOrNull()
        if (id == null) {
            call.respond(HttpStatusCode.NotFound)
            return@get
        }
        val result = withContext(Dispatchers.IO) { db.transaction { it.catalogManga(id) } }
        if (result == null) call.respond(HttpStatusCode.NotFound) else call.respond(result)
    }
}
