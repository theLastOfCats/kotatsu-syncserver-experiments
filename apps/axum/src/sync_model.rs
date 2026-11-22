use serde::{Deserialize, Serialize};

fn required_nullable<'de, D, T>(deserializer: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    Option::deserialize(deserializer)
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq, PartialOrd, Ord)]
pub struct Tag {
    #[serde(rename = "tag_id")]
    pub id: i64,
    pub title: String,
    pub key: String,
    pub source: String,
    #[serde(default)]
    pub pinned: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct Manga {
    #[serde(rename = "manga_id")]
    pub id: i64,
    pub title: String,
    #[serde(deserialize_with = "required_nullable")]
    pub alt_title: Option<String>,
    pub url: String,
    pub public_url: String,
    pub rating: f32,
    #[serde(default)]
    pub content_rating: Option<String>,
    pub cover_url: String,
    #[serde(deserialize_with = "required_nullable")]
    pub large_cover_url: Option<String>,
    pub tags: Vec<Tag>,
    #[serde(deserialize_with = "required_nullable")]
    pub state: Option<String>,
    #[serde(deserialize_with = "required_nullable")]
    pub author: Option<String>,
    pub source: String,
    #[serde(default)]
    pub nsfw: bool,
}

// The Kotlin model uses a Set for tags and truncates strings before storage.
// Keep the request's other values intact for the content-equality decision.
impl Manga {
    pub fn canonicalize_tags(&mut self) {
        self.tags.sort();
        self.tags.dedup();
    }

    pub fn stored(mut self) -> Self {
        truncate(&mut self.title, 100);
        truncate_option(&mut self.alt_title, 100);
        truncate(&mut self.url, 255);
        truncate(&mut self.public_url, 255);
        truncate(&mut self.cover_url, 255);
        truncate_option(&mut self.large_cover_url, 255);
        truncate_option(&mut self.author, 64);
        truncate(&mut self.source, 32);
        if !matches!(
            self.content_rating.as_deref(),
            Some("SAFE" | "SUGGESTIVE" | "ADULT")
        ) {
            self.content_rating = None;
        }
        if !matches!(
            self.state.as_deref(),
            Some("ONGOING" | "FINISHED" | "ABANDONED" | "PAUSED" | "UPCOMING")
        ) {
            self.state = None;
        }
        if self.large_cover_url.is_none() {
            // The reference reads this nullable database column as a non-null string.
            self.large_cover_url = Some(String::new());
        }
        self
    }
}

impl Tag {
    pub fn stored(mut self) -> Self {
        truncate(&mut self.title, 64);
        truncate(&mut self.key, 120);
        truncate(&mut self.source, 32);
        self
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct History {
    pub manga_id: i64,
    pub manga: Manga,
    #[serde(flatten)]
    pub data: HistoryData,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct HistoryData {
    pub created_at: i64,
    pub updated_at: i64,
    pub chapter_id: i64,
    pub page: i32,
    pub scroll: f32,
    pub percent: f32,
    #[serde(default = "default_chapters")]
    pub chapters: i32,
    pub deleted_at: i64,
}

fn default_chapters() -> i32 {
    -1
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct HistoryPackage {
    pub history: Vec<History>,
    #[serde(deserialize_with = "required_nullable")]
    pub timestamp: Option<i64>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct Category {
    #[serde(rename = "category_id")]
    pub id: i32,
    pub created_at: i64,
    pub sort_key: i32,
    pub track: bool,
    pub title: String,
    pub order: String,
    pub deleted_at: i64,
    pub show_in_lib: bool,
}

impl Category {
    pub fn stored(mut self) -> Self {
        truncate(&mut self.title, 120);
        self
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct Favourite {
    pub manga_id: i64,
    pub manga: Manga,
    pub category_id: i32,
    #[serde(flatten)]
    pub data: FavouriteData,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct FavouriteData {
    pub sort_key: i32,
    pub pinned: bool,
    pub created_at: i64,
    pub deleted_at: i64,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct FavouritesPackage {
    pub categories: Vec<Category>,
    pub favourites: Vec<Favourite>,
    #[serde(deserialize_with = "required_nullable")]
    pub timestamp: Option<i64>,
}

fn truncate(value: &mut String, max_utf16_units: usize) {
    let mut units = 0;
    let end = value.char_indices().find_map(|(index, ch)| {
        units += ch.len_utf16();
        (units > max_utf16_units).then_some(index)
    });
    if let Some(end) = end {
        value.truncate(end);
    }
}

fn truncate_option(value: &mut Option<String>, max_utf16_units: usize) {
    if let Some(value) = value {
        truncate(value, max_utf16_units);
    }
}
