#[path = "sync_model.rs"]
mod model;
#[path = "sync_store.rs"]
mod store;

use std::collections::HashMap;

use axum::{
    extract::{rejection::JsonRejection, Path, Query, State},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::get,
    Json, Router,
};

use crate::{auth, AppState};
use model::{FavouritesPackage, HistoryPackage};

pub use store::migrate;

pub fn routes() -> Router<AppState> {
    Router::new()
        .route("/resource/history", get(get_history).post(post_history))
        .route(
            "/resource/favourites",
            get(get_favourites).post(post_favourites),
        )
        .route("/manga", get(get_catalog))
        .route("/manga/{id}", get(get_manga))
}

fn storage_error(error: store::Error) -> StatusCode {
    match error {
        store::Error::Invalid => StatusCode::BAD_REQUEST,
        store::Error::Database(error) => {
            eprintln!("sync database error: {error}");
            StatusCode::INTERNAL_SERVER_ERROR
        }
    }
}

async fn get_history(
    State(state): State<AppState>,
    headers: HeaderMap,
) -> Result<Response, StatusCode> {
    let user = auth::current_user(&headers, &state).await?;
    let response = store::history(&state.pool, &state.dialect, user, None)
        .await
        .map_err(storage_error)?;
    Ok(Json(response).into_response())
}

async fn post_history(
    State(state): State<AppState>,
    headers: HeaderMap,
    request: Result<Json<HistoryPackage>, JsonRejection>,
) -> Result<Response, StatusCode> {
    let user = auth::current_user(&headers, &state).await?;
    let mut request = request.map_err(|_| StatusCode::BAD_REQUEST)?.0;
    for row in &mut request.history {
        row.manga.canonicalize_tags();
    }
    let response = store::history(&state.pool, &state.dialect, user, Some(&request))
        .await
        .map_err(storage_error)?;
    if response.history == request.history {
        Ok(StatusCode::NO_CONTENT.into_response())
    } else {
        Ok(Json(response).into_response())
    }
}

async fn get_favourites(
    State(state): State<AppState>,
    headers: HeaderMap,
) -> Result<Response, StatusCode> {
    let user = auth::current_user(&headers, &state).await?;
    let response = store::favourites(&state.pool, &state.dialect, user, None)
        .await
        .map_err(storage_error)?;
    Ok(Json(response).into_response())
}

async fn post_favourites(
    State(state): State<AppState>,
    headers: HeaderMap,
    request: Result<Json<FavouritesPackage>, JsonRejection>,
) -> Result<Response, StatusCode> {
    let user = auth::current_user(&headers, &state).await?;
    let mut request = request.map_err(|_| StatusCode::BAD_REQUEST)?.0;
    for row in &mut request.favourites {
        row.manga.canonicalize_tags();
    }
    let response = store::favourites(&state.pool, &state.dialect, user, Some(&request))
        .await
        .map_err(storage_error)?;
    if response.categories == request.categories && response.favourites == request.favourites {
        Ok(StatusCode::NO_CONTENT.into_response())
    } else {
        Ok(Json(response).into_response())
    }
}

async fn get_catalog(
    State(state): State<AppState>,
    Query(params): Query<HashMap<String, String>>,
) -> Result<Response, StatusCode> {
    let offset = params
        .get("offset")
        .and_then(|s| s.parse::<i32>().ok())
        .filter(|n| *n >= 0)
        .ok_or(StatusCode::BAD_REQUEST)?;
    let limit = params
        .get("limit")
        .and_then(|s| s.parse::<i32>().ok())
        .filter(|n| *n >= 0)
        .ok_or(StatusCode::BAD_REQUEST)?;
    let response = store::catalog(&state.pool, i64::from(offset), i64::from(limit))
        .await
        .map_err(storage_error)?;
    Ok(Json(response).into_response())
}

async fn get_manga(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> Result<Response, StatusCode> {
    let id = id.parse::<i64>().map_err(|_| StatusCode::NOT_FOUND)?;
    let response = store::manga(&state.pool, id)
        .await
        .map_err(storage_error)?
        .ok_or(StatusCode::NOT_FOUND)?;
    Ok(Json(response).into_response())
}
