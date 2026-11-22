mod auth;
mod config;
mod db;
mod sync;

use axum::{
    extract::{ConnectInfo, DefaultBodyLimit, Query, Request, State},
    http::{header::RETRY_AFTER, uri::PathAndQuery, Method, StatusCode, Uri},
    middleware::{self, Next},
    response::{Html, IntoResponse, Response},
    routing::get,
    Router,
};
use sqlx::AnyPool;
use std::{
    collections::HashMap,
    net::{IpAddr, SocketAddr},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tokio::sync::Semaphore;
use tower_http::compression::CompressionLayer;

#[derive(Clone)]
pub struct AppState {
    pub pool: AnyPool,
    pub dialect: String,
    config: Arc<config::Config>,
    limiter: RateLimiter,
    mailer: auth::Mailer,
    hash_slots: Arc<Semaphore>,
}

#[derive(Clone, Copy, Eq, PartialEq, Hash)]
enum RateKind {
    Auth,
    Forgot,
    Reset,
}

impl RateKind {
    fn window(self) -> Duration {
        match self {
            Self::Auth => Duration::from_secs(60),
            Self::Forgot | Self::Reset => Duration::from_secs(600),
        }
    }
}

struct RateData {
    entries: HashMap<(IpAddr, RateKind), Window>,
    global: Window,
    cleaned: Instant,
}

struct Window {
    started: Instant,
    count: usize,
}

impl Window {
    fn allow(&mut self, now: Instant, duration: Duration, limit: usize) -> Option<u64> {
        let elapsed = now.duration_since(self.started);
        if elapsed >= duration {
            self.started = now;
            self.count = 0;
        } else if self.count >= limit {
            let remaining = duration - elapsed;
            return Some(remaining.as_secs() + u64::from(remaining.subsec_nanos() != 0));
        }
        self.count += 1;
        None
    }
}

#[derive(Clone)]
struct RateLimiter(Arc<Mutex<RateData>>);

impl RateLimiter {
    fn new() -> Self {
        Self(Arc::new(Mutex::new(RateData {
            entries: HashMap::new(),
            global: Window { started: Instant::now(), count: 0 },
            cleaned: Instant::now(),
        })))
    }

    fn allow(&self, ip: IpAddr, kind: Option<RateKind>, config: &config::Config) -> Option<u64> {
        let now = Instant::now();
        let mut data = self.0.lock().expect("rate limiter mutex poisoned");
        if let Some(wait) = data.global.allow(now, Duration::from_secs(1), config.api_rate_limit) {
            return Some(wait);
        }
        if now.duration_since(data.cleaned) >= Duration::from_secs(60) {
            data.entries.retain(|(_, kind), window| now.duration_since(window.started) < kind.window());
            data.cleaned = now;
        }
        let kind = kind?;
        let limit = match kind {
            RateKind::Auth => config.auth_rate_limit,
            RateKind::Forgot => config.forgot_rate_limit,
            RateKind::Reset => config.reset_rate_limit,
        };
        data.entries
            .entry((ip, kind))
            .or_insert(Window { started: now, count: 0 })
            .allow(now, kind.window(), limit)
    }
}

async fn limit_api(State(state): State<AppState>, request: Request, next: Next) -> Response {
    let kind = if request.method() == Method::POST {
        match request.uri().path() {
            "/auth" => Some(RateKind::Auth),
            "/forgot-password" => Some(RateKind::Forgot),
            "/reset-password" => Some(RateKind::Reset),
            _ => None,
        }
    } else {
        None
    };
    let ip = request.extensions().get::<ConnectInfo<SocketAddr>>()
        .expect("connection information missing").0.ip();
    if let Some(wait) = state.limiter.allow(ip, kind, &state.config) {
        return (StatusCode::TOO_MANY_REQUESTS, [(RETRY_AFTER, wait.to_string())]).into_response();
    }
    next.run(request).await
}

fn html_escape(value: &str) -> String {
    let mut escaped = String::with_capacity(value.len());
    for ch in value.chars() {
        match ch {
            '&' => escaped.push_str("&amp;"),
            '<' => escaped.push_str("&lt;"),
            '>' => escaped.push_str("&gt;"),
            '"' => escaped.push_str("&quot;"),
            '\'' => escaped.push_str("&#39;"),
            _ => escaped.push(ch),
        }
    }
    escaped
}

async fn reset_deeplink(
    State(state): State<AppState>,
    Query(query): Query<HashMap<String, String>>,
) -> Response {
    let Some(token) = query.get("token").filter(|token| !token.trim().is_empty()) else {
        return (StatusCode::BAD_REQUEST, "Missing token").into_response();
    };
    let link = format!(
        "kotatsu://reset-password?base_url={}&token={token}",
        state.config.base_url
    );
    let js_link = serde_json::to_string(&link)
        .expect("serializing a string cannot fail")
        .replace('<', "\\u003c")
        .replace('>', "\\u003e");
    let html = include_str!("../../../src/main/resources/templates/pages/reset-password.hbs")
        .replace("\"{{{deep_link}}}\"", &js_link)
        .replace("{{deep_link}}", &html_escape(&link));
    Html(html).into_response()
}

async fn normalize_path(mut request: Request, next: Next) -> Response {
    let path = request.uri().path();
    if path.contains("//") {
        let mut normalized = String::with_capacity(path.len());
        let mut previous_slash = false;
        for ch in path.chars() {
            if ch == '/' {
                if !previous_slash {
                    normalized.push(ch);
                }
                previous_slash = true;
            } else {
                normalized.push(ch);
                previous_slash = false;
            }
        }
        if let Some(query) = request.uri().query() {
            normalized.push('?');
            normalized.push_str(query);
        }
        if let Ok(path_and_query) = normalized.parse::<PathAndQuery>() {
            let mut parts = request.uri().clone().into_parts();
            parts.path_and_query = Some(path_and_query);
            if let Ok(uri) = Uri::from_parts(parts) {
                *request.uri_mut() = uri;
            }
        }
    }
    next.run(request).await
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let config = config::Config::from_env()?;
    let mailer = auth::Mailer::new(&config.mail)?;
    let port = config.port;
    let (pool, dialect) = db::connect().await?;
    db::migrate(&pool, &dialect).await?;
    sync::migrate(&pool, &dialect).await?;
    let hash_slots = Arc::new(Semaphore::new(config.password_concurrency));
    let state = AppState {
        pool,
        dialect,
        config: Arc::new(config),
        limiter: RateLimiter::new(),
        mailer,
        hash_slots,
    };
    let api = Router::new()
        .merge(auth::routes())
        .merge(sync::routes())
        .route_layer(middleware::from_fn_with_state(state.clone(), limit_api));
    let routes = Router::new()
        .route("/", get(|| async { "Alive" }))
        .route("/deeplink/reset-password", get(reset_deeplink))
        .merge(api)
        .layer(DefaultBodyLimit::max(64 * 1024 * 1024))
        .with_state(state);
    // A fallback service is used so URI normalization runs before the inner router matches.
    let app = Router::new()
        .fallback_service(routes)
        .layer(middleware::from_fn(normalize_path))
        .layer(CompressionLayer::new());
    let listener = tokio::net::TcpListener::bind(("127.0.0.1", port)).await?;
    axum::serve(
        listener,
        app.into_make_service_with_connect_info::<SocketAddr>(),
    )
    .with_graceful_shutdown(async {
        let _ = tokio::signal::ctrl_c().await;
    })
    .await?;
    Ok(())
}
