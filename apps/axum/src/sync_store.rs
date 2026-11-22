use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
use std::time::{SystemTime, UNIX_EPOCH};

use serde::{de::DeserializeOwned, Serialize};
use sqlx::{Any, AnyPool, AssertSqlSafe, QueryBuilder, Row, Transaction};

use super::model::{
    Category, Favourite, FavouriteData, FavouritesPackage, History, HistoryData, HistoryPackage,
    Manga, Tag,
};

const BATCH: usize = 200;
type Tx<'a> = Transaction<'a, Any>;

#[derive(Debug)]
pub enum Error {
    Database(sqlx::Error),
    Invalid,
}

impl From<sqlx::Error> for Error {
    fn from(error: sqlx::Error) -> Self {
        Self::Database(error)
    }
}

fn encode<T: Serialize>(value: &T) -> Result<String, Error> {
    serde_json::to_string(value).map_err(|e| Error::Database(sqlx::Error::Encode(Box::new(e))))
}

fn decode<T: DeserializeOwned>(value: &str) -> Result<T, Error> {
    serde_json::from_str(value).map_err(|e| Error::Database(sqlx::Error::Decode(Box::new(e))))
}

fn mysql(dialect: &str) -> bool {
    dialect == "mysql"
}

pub async fn migrate(pool: &AnyPool, dialect: &str) -> Result<(), sqlx::Error> {
    // SQLx Any treats MySQL TEXT as bytes; bounded DTO documents fit in VARCHAR.
    let text = if mysql(dialect) {
        "VARCHAR(10000)"
    } else {
        "TEXT"
    };
    let statements = [
        format!("CREATE TABLE IF NOT EXISTS sync_manga (manga_id BIGINT PRIMARY KEY, doc {text} NOT NULL)"),
        format!("CREATE TABLE IF NOT EXISTS sync_tags (tag_id BIGINT PRIMARY KEY, doc {text} NOT NULL)"),
        "CREATE TABLE IF NOT EXISTS sync_manga_tags (manga_id BIGINT NOT NULL, tag_id BIGINT NOT NULL, PRIMARY KEY (manga_id, tag_id))".to_owned(),
        "CREATE TABLE IF NOT EXISTS sync_meta (user_id BIGINT PRIMARY KEY, favourites_ts BIGINT NOT NULL, history_ts BIGINT NOT NULL, FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE)".to_owned(),
        format!("CREATE TABLE IF NOT EXISTS sync_history (user_id BIGINT NOT NULL, manga_id BIGINT NOT NULL, doc {text} NOT NULL, PRIMARY KEY (user_id, manga_id), FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE)"),
        format!("CREATE TABLE IF NOT EXISTS sync_categories (user_id BIGINT NOT NULL, category_id BIGINT NOT NULL, doc {text} NOT NULL, PRIMARY KEY (user_id, category_id), FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE)"),
        format!("CREATE TABLE IF NOT EXISTS sync_favourites (user_id BIGINT NOT NULL, manga_id BIGINT NOT NULL, category_id BIGINT NOT NULL, doc {text} NOT NULL, PRIMARY KEY (user_id, manga_id, category_id), FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE)"),
    ];
    for statement in statements {
        sqlx::query(AssertSqlSafe(statement)).execute(pool).await?;
    }
    Ok(())
}

// Acquiring this row before reading the account snapshot serializes all of an
// account's merges. On SQLite, even an ignored insert obtains the writer lock.
async fn lock_account(tx: &mut Tx<'_>, dialect: &str, user_id: i64) -> Result<(), Error> {
    let sql = if mysql(dialect) {
        "INSERT INTO sync_meta (user_id, favourites_ts, history_ts) VALUES (?, 0, 0) ON DUPLICATE KEY UPDATE user_id = user_id"
    } else {
        "INSERT INTO sync_meta (user_id, favourites_ts, history_ts) VALUES (?, 0, 0) ON CONFLICT(user_id) DO UPDATE SET user_id = excluded.user_id"
    };
    sqlx::query(sql).bind(user_id).execute(&mut **tx).await?;
    Ok(())
}

async fn timestamps(tx: &mut Tx<'_>, user_id: i64) -> Result<(i64, i64), Error> {
    let row = sqlx::query("SELECT favourites_ts, history_ts FROM sync_meta WHERE user_id = ?")
        .bind(user_id)
        .fetch_optional(&mut **tx)
        .await?;
    match row {
        Some(row) => Ok((row.try_get("favourites_ts")?, row.try_get("history_ts")?)),
        None => Ok((0, 0)),
    }
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .min(i64::MAX as u128) as i64
}

async fn update_timestamp(tx: &mut Tx<'_>, user_id: i64, history: bool) -> Result<(), Error> {
    let sql = if history {
        "UPDATE sync_meta SET history_ts = ? WHERE user_id = ?"
    } else {
        "UPDATE sync_meta SET favourites_ts = ? WHERE user_id = ?"
    };
    sqlx::query(sql)
        .bind(now_ms())
        .bind(user_id)
        .execute(&mut **tx)
        .await?;
    Ok(())
}

// Attach tags for the whole result in bounded queries, not once per manga.
async fn attach_tags<'a>(
    tx: &mut Tx<'_>,
    mangas: impl IntoIterator<Item = &'a mut Manga>,
) -> Result<(), Error> {
    let mangas: Vec<&mut Manga> = mangas.into_iter().collect();
    let mut ids: Vec<i64> = mangas.iter().map(|m| m.id).collect();
    ids.sort_unstable();
    ids.dedup();
    let mut tags: HashMap<i64, Vec<Tag>> = HashMap::new();
    for chunk in ids.chunks(BATCH) {
        let mut query = QueryBuilder::<Any>::new(
            "SELECT mt.manga_id, t.doc FROM sync_manga_tags mt JOIN sync_tags t ON t.tag_id = mt.tag_id WHERE mt.manga_id IN ("
        );
        {
            let mut separated = query.separated(", ");
            for id in chunk {
                separated.push_bind(*id);
            }
        }
        query.push(") ORDER BY mt.manga_id, mt.tag_id");
        for row in query.build().fetch_all(&mut **tx).await? {
            let id: i64 = row.try_get("manga_id")?;
            let doc: String = row.try_get("doc")?;
            tags.entry(id).or_default().push(decode(&doc)?);
        }
    }
    for manga in mangas {
        manga.tags = tags.get(&manga.id).cloned().unwrap_or_default();
        manga.canonicalize_tags();
    }
    Ok(())
}

async fn load_history(tx: &mut Tx<'_>, user_id: i64) -> Result<Vec<History>, Error> {
    let rows = sqlx::query("SELECT h.manga_id, h.doc AS row_doc, m.doc AS manga_doc FROM sync_history h JOIN sync_manga m ON m.manga_id = h.manga_id WHERE h.user_id = ? ORDER BY h.manga_id")
        .bind(user_id).fetch_all(&mut **tx).await?;
    let mut history = Vec::with_capacity(rows.len());
    for row in rows {
        let doc: String = row.try_get("row_doc")?;
        let manga_doc: String = row.try_get("manga_doc")?;
        history.push(History {
            manga_id: row.try_get("manga_id")?,
            manga: decode(&manga_doc)?,
            data: decode(&doc)?,
        });
    }
    attach_tags(tx, history.iter_mut().map(|row| &mut row.manga)).await?;
    Ok(history)
}

async fn load_categories(tx: &mut Tx<'_>, user_id: i64) -> Result<Vec<Category>, Error> {
    let rows =
        sqlx::query("SELECT doc FROM sync_categories WHERE user_id = ? ORDER BY category_id")
            .bind(user_id)
            .fetch_all(&mut **tx)
            .await?;
    rows.into_iter()
        .map(|row| {
            let doc: String = row.try_get("doc")?;
            decode(&doc)
        })
        .collect()
}

async fn load_favourites(tx: &mut Tx<'_>, user_id: i64) -> Result<Vec<Favourite>, Error> {
    let rows = sqlx::query("SELECT f.manga_id, f.category_id, f.doc AS row_doc, m.doc AS manga_doc FROM sync_favourites f JOIN sync_manga m ON m.manga_id = f.manga_id WHERE f.user_id = ? ORDER BY f.manga_id, f.category_id")
        .bind(user_id).fetch_all(&mut **tx).await?;
    let mut favourites = Vec::with_capacity(rows.len());
    for row in rows {
        let doc: String = row.try_get("row_doc")?;
        let manga_doc: String = row.try_get("manga_doc")?;
        let category_id: i64 = row.try_get("category_id")?;
        favourites.push(Favourite {
            manga_id: row.try_get("manga_id")?,
            category_id: category_id as i32,
            manga: decode(&manga_doc)?,
            data: decode(&doc)?,
        });
    }
    attach_tags(tx, favourites.iter_mut().map(|row| &mut row.manga)).await?;
    Ok(favourites)
}

fn suffix<'a>(dialect: &str, sqlite: &'a str, mysql_sql: &'a str) -> &'a str {
    if mysql(dialect) {
        mysql_sql
    } else {
        sqlite
    }
}

// Input is folded by key before writing; sorted keys also give concurrent
// MySQL transactions a consistent lock order for the shared catalog and tags.
async fn save_manga<'a>(
    tx: &mut Tx<'_>,
    dialect: &str,
    uploaded: impl Iterator<Item = &'a Manga>,
    old: impl Iterator<Item = &'a Manga>,
) -> Result<bool, Error> {
    let mut old_manga = HashMap::new();
    let mut old_tags = HashMap::new();
    let mut old_links = HashSet::new();
    for manga in old {
        let mut card = manga.clone();
        for tag in card.tags.drain(..) {
            old_links.insert((card.id, tag.id));
            old_tags.insert(tag.id, tag);
        }
        old_manga.insert(card.id, card);
    }
    let mut cards = BTreeMap::new();
    let mut tags = BTreeMap::new();
    let mut links = BTreeSet::new();
    for manga in uploaded {
        let mut card = manga.clone().stored();
        for tag in card.tags.drain(..) {
            links.insert((card.id, tag.id));
            tags.insert(tag.id, tag.stored());
        }
        cards.insert(card.id, card);
    }
    let mut card_docs = Vec::new();
    for (id, card) in cards {
        if old_manga.get(&id) != Some(&card) {
            card_docs.push((id, encode(&card)?));
        }
    }
    for chunk in card_docs.chunks(BATCH) {
        let mut query = QueryBuilder::<Any>::new("INSERT INTO sync_manga (manga_id, doc) ");
        query.push_values(chunk, |mut row, (id, doc)| {
            row.push_bind(*id).push_bind(doc.as_str());
        });
        query.push(suffix(
            dialect,
            " ON CONFLICT(manga_id) DO UPDATE SET doc = excluded.doc",
            " ON DUPLICATE KEY UPDATE doc = VALUES(doc)",
        ));
        query.build().execute(&mut **tx).await?;
    }
    let mut tag_docs = Vec::new();
    for (id, tag) in tags {
        if old_tags.get(&id) != Some(&tag) {
            tag_docs.push((id, encode(&tag)?));
        }
    }
    for chunk in tag_docs.chunks(BATCH) {
        let mut query = QueryBuilder::<Any>::new("INSERT INTO sync_tags (tag_id, doc) ");
        query.push_values(chunk, |mut row, (id, doc)| {
            row.push_bind(*id).push_bind(doc.as_str());
        });
        query.push(suffix(
            dialect,
            " ON CONFLICT(tag_id) DO UPDATE SET doc = excluded.doc",
            " ON DUPLICATE KEY UPDATE doc = VALUES(doc)",
        ));
        query.build().execute(&mut **tx).await?;
    }
    let new_links: Vec<_> = links
        .into_iter()
        .filter(|link| !old_links.contains(link))
        .collect();
    let changed = !card_docs.is_empty() || !tag_docs.is_empty() || !new_links.is_empty();
    for chunk in new_links.chunks(BATCH) {
        let mut query = QueryBuilder::<Any>::new("INSERT INTO sync_manga_tags (manga_id, tag_id) ");
        query.push_values(chunk, |mut row, (manga_id, tag_id)| {
            row.push_bind(*manga_id).push_bind(*tag_id);
        });
        query.push(suffix(
            dialect,
            " ON CONFLICT(manga_id, tag_id) DO NOTHING",
            " ON DUPLICATE KEY UPDATE tag_id = VALUES(tag_id)",
        ));
        query.build().execute(&mut **tx).await?;
    }
    Ok(changed)
}

async fn save_history(
    tx: &mut Tx<'_>,
    dialect: &str,
    user_id: i64,
    rows: &BTreeMap<i64, HistoryData>,
) -> Result<(), Error> {
    let docs: Vec<_> = rows
        .iter()
        .map(|(&id, data)| Ok((id, encode(data)?)))
        .collect::<Result<_, Error>>()?;
    for chunk in docs.chunks(BATCH) {
        let mut query =
            QueryBuilder::<Any>::new("INSERT INTO sync_history (user_id, manga_id, doc) ");
        query.push_values(chunk, |mut row, (id, doc)| {
            row.push_bind(user_id)
                .push_bind(*id)
                .push_bind(doc.as_str());
        });
        query.push(suffix(
            dialect,
            " ON CONFLICT(user_id, manga_id) DO UPDATE SET doc = excluded.doc",
            " ON DUPLICATE KEY UPDATE doc = VALUES(doc)",
        ));
        query.build().execute(&mut **tx).await?;
    }
    Ok(())
}

async fn save_categories(
    tx: &mut Tx<'_>,
    dialect: &str,
    user_id: i64,
    rows: &BTreeMap<i32, Category>,
) -> Result<(), Error> {
    let docs: Vec<_> = rows
        .iter()
        .map(|(&id, data)| Ok((id, encode(data)?)))
        .collect::<Result<_, Error>>()?;
    for chunk in docs.chunks(BATCH) {
        let mut query =
            QueryBuilder::<Any>::new("INSERT INTO sync_categories (user_id, category_id, doc) ");
        query.push_values(chunk, |mut row, (id, doc)| {
            row.push_bind(user_id)
                .push_bind(i64::from(*id))
                .push_bind(doc.as_str());
        });
        query.push(suffix(
            dialect,
            " ON CONFLICT(user_id, category_id) DO UPDATE SET doc = excluded.doc",
            " ON DUPLICATE KEY UPDATE doc = VALUES(doc)",
        ));
        query.build().execute(&mut **tx).await?;
    }
    Ok(())
}

async fn save_favourites(
    tx: &mut Tx<'_>,
    dialect: &str,
    user_id: i64,
    rows: &BTreeMap<(i64, i32), FavouriteData>,
) -> Result<(), Error> {
    let docs: Vec<_> = rows
        .iter()
        .map(|(&(manga, category), data)| Ok((manga, category, encode(data)?)))
        .collect::<Result<_, Error>>()?;
    for chunk in docs.chunks(BATCH) {
        let mut query = QueryBuilder::<Any>::new(
            "INSERT INTO sync_favourites (user_id, manga_id, category_id, doc) ",
        );
        query.push_values(chunk, |mut row, (manga, category, doc)| {
            row.push_bind(user_id)
                .push_bind(*manga)
                .push_bind(i64::from(*category))
                .push_bind(doc.as_str());
        });
        query.push(suffix(
            dialect,
            " ON CONFLICT(user_id, manga_id, category_id) DO UPDATE SET doc = excluded.doc",
            " ON DUPLICATE KEY UPDATE doc = VALUES(doc)",
        ));
        query.build().execute(&mut **tx).await?;
    }
    Ok(())
}

// Existing keys can be updated in the ordered snapshot without reading it
// again. A key left in `changes` is an insert and needs the database readback.
fn patch_snapshot<T, K: Ord, V>(
    snapshot: &mut [T],
    mut changes: BTreeMap<K, V>,
    key: impl Fn(&T) -> K,
    apply: impl Fn(&mut T, V),
) -> bool {
    for row in snapshot {
        if changes.is_empty() {
            return true;
        }
        if let Some(value) = changes.remove(&key(row)) {
            apply(row, value);
        }
    }
    changes.is_empty()
}

// The deletion timestamp is a real event even when updated_at did not change.
// On equal event times a tombstone wins, and later reading can restore it.
fn wins(new_changed: i64, new_deleted: i64, old_changed: i64, old_deleted: i64) -> bool {
    let new_time = new_changed.max(new_deleted);
    let old_time = old_changed.max(old_deleted);
    new_time > old_time || (new_time == old_time && (new_deleted != 0 || old_deleted == 0))
}

// Two live copies have no modification clock. Two tombstones must not move a
// deletion backwards, or a stale re-add can beat the weakened tombstone.
fn wins_favourite(new_changed: i64, new_deleted: i64, old_changed: i64, old_deleted: i64) -> bool {
    match (new_deleted, old_deleted) {
        (0, 0) => true,
        (0, _) | (_, 0) => wins(new_changed, new_deleted, old_changed, old_deleted),
        (new, old) => new >= old,
    }
}

pub async fn history(
    pool: &AnyPool,
    dialect: &str,
    user_id: i64,
    request: Option<&HistoryPackage>,
) -> Result<HistoryPackage, Error> {
    if let Some(request) = request {
        if request
            .history
            .iter()
            .any(|row| row.manga_id != row.manga.id || i16::try_from(row.data.page).is_err())
        {
            return Err(Error::Invalid);
        }
    }
    let mut tx = pool.begin().await?;
    if request.is_some() {
        lock_account(&mut tx, dialect, user_id).await?;
    }
    let (_, timestamp) = timestamps(&mut tx, user_id).await?;
    let mut old = load_history(&mut tx, user_id).await?;
    let rows = if let Some(request) = request {
        let mut current: HashMap<i64, HistoryData> = old
            .iter()
            .map(|row| (row.manga_id, row.data.clone()))
            .collect();
        let mut changed = BTreeMap::new();
        for row in &request.history {
            let replace = current.get(&row.manga_id).map_or(true, |prior| {
                wins(
                    row.data.updated_at,
                    row.data.deleted_at,
                    prior.updated_at,
                    prior.deleted_at,
                )
            });
            if replace && current.get(&row.manga_id) != Some(&row.data) {
                current.insert(row.manga_id, row.data.clone());
                changed.insert(row.manga_id, row.data.clone());
            }
        }
        let manga_changed = save_manga(
            &mut tx,
            dialect,
            request.history.iter().map(|row| &row.manga),
            old.iter().map(|row| &row.manga),
        )
        .await?;
        save_history(&mut tx, dialect, user_id, &changed).await?;
        // JSON cannot round-trip non-finite floats. Keep the storage readback
        // (and rollback on decode failure) if encoding could have changed one.
        let reuse = !manga_changed
            && changed
                .values()
                .all(|row| row.scroll.is_finite() && row.percent.is_finite())
            && patch_snapshot(&mut old, changed, |row| row.manga_id, |row, data| row.data = data);
        let rows = if reuse {
            old
        } else {
            load_history(&mut tx, user_id).await?
        };
        update_timestamp(&mut tx, user_id, true).await?;
        rows
    } else {
        old
    };
    tx.commit().await?;
    Ok(HistoryPackage {
        history: rows,
        timestamp: Some(timestamp),
    })
}

pub async fn favourites(
    pool: &AnyPool,
    dialect: &str,
    user_id: i64,
    request: Option<&FavouritesPackage>,
) -> Result<FavouritesPackage, Error> {
    if let Some(request) = request {
        if request
            .favourites
            .iter()
            .any(|row| row.manga_id != row.manga.id)
            || request
                .categories
                .iter()
                .any(|category| category.order.chars().count() > 16)
        {
            return Err(Error::Invalid);
        }
    }
    let mut tx = pool.begin().await?;
    if request.is_some() {
        lock_account(&mut tx, dialect, user_id).await?;
    }
    let (timestamp, _) = timestamps(&mut tx, user_id).await?;
    let mut old_categories = load_categories(&mut tx, user_id).await?;
    let mut old_favourites = load_favourites(&mut tx, user_id).await?;
    let (categories, favourites) = if let Some(request) = request {
        let mut current_categories: HashMap<i32, Category> = old_categories
            .iter()
            .map(|row| (row.id, row.clone()))
            .collect();
        let mut changed_categories = BTreeMap::new();
        for row in &request.categories {
            let row = row.clone().stored();
            let replace = current_categories.get(&row.id).map_or(true, |prior| {
                wins_favourite(
                    row.created_at,
                    row.deleted_at,
                    prior.created_at,
                    prior.deleted_at,
                )
            });
            if replace && current_categories.get(&row.id) != Some(&row) {
                current_categories.insert(row.id, row.clone());
                changed_categories.insert(row.id, row);
            }
        }
        if request
            .favourites
            .iter()
            .any(|row| !current_categories.contains_key(&row.category_id))
        {
            return Err(Error::Invalid);
        }
        let mut current: HashMap<(i64, i32), FavouriteData> = old_favourites
            .iter()
            .map(|row| ((row.manga_id, row.category_id), row.data.clone()))
            .collect();
        let mut changed = BTreeMap::new();
        for row in &request.favourites {
            let key = (row.manga_id, row.category_id);
            let replace = current.get(&key).map_or(true, |prior| {
                wins_favourite(
                    row.data.created_at,
                    row.data.deleted_at,
                    prior.created_at,
                    prior.deleted_at,
                )
            });
            if replace && current.get(&key) != Some(&row.data) {
                current.insert(key, row.data.clone());
                changed.insert(key, row.data.clone());
            }
        }
        save_categories(&mut tx, dialect, user_id, &changed_categories).await?;
        let manga_changed = save_manga(
            &mut tx,
            dialect,
            request.favourites.iter().map(|row| &row.manga),
            old_favourites.iter().map(|row| &row.manga),
        )
        .await?;
        save_favourites(&mut tx, dialect, user_id, &changed).await?;
        let categories = if patch_snapshot(
            &mut old_categories,
            changed_categories,
            |row| row.id,
            |row, category| *row = category,
        ) {
            old_categories
        } else {
            load_categories(&mut tx, user_id).await?
        };
        let favourites = if !manga_changed
            && patch_snapshot(
                &mut old_favourites,
                changed,
                |row| (row.manga_id, row.category_id),
                |row, data| row.data = data,
            ) {
            old_favourites
        } else {
            load_favourites(&mut tx, user_id).await?
        };
        update_timestamp(&mut tx, user_id, false).await?;
        (categories, favourites)
    } else {
        (old_categories, old_favourites)
    };
    tx.commit().await?;
    Ok(FavouritesPackage {
        categories,
        favourites,
        timestamp: Some(timestamp),
    })
}

pub async fn catalog(pool: &AnyPool, offset: i64, limit: i64) -> Result<Vec<Manga>, Error> {
    let mut tx = pool.begin().await?;
    let rows = sqlx::query("SELECT doc FROM sync_manga ORDER BY manga_id LIMIT ? OFFSET ?")
        .bind(limit)
        .bind(offset)
        .fetch_all(&mut *tx)
        .await?;
    let mut mangas = Vec::with_capacity(rows.len());
    for row in rows {
        let doc: String = row.try_get("doc")?;
        mangas.push(decode(&doc)?);
    }
    attach_tags(&mut tx, mangas.iter_mut()).await?;
    tx.commit().await?;
    Ok(mangas)
}

pub async fn manga(pool: &AnyPool, id: i64) -> Result<Option<Manga>, Error> {
    let mut tx = pool.begin().await?;
    let row = sqlx::query("SELECT doc FROM sync_manga WHERE manga_id = ?")
        .bind(id)
        .fetch_optional(&mut *tx)
        .await?;
    let manga = if let Some(row) = row {
        let doc: String = row.try_get("doc")?;
        let mut mangas = vec![decode(&doc)?];
        attach_tags(&mut tx, mangas.iter_mut()).await?;
        mangas.pop()
    } else {
        None
    };
    tx.commit().await?;
    Ok(manga)
}
