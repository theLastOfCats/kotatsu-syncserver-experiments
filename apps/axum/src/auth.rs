use crate::{config::MailConfig, html_escape, AppState};
use argon2::{
    password_hash::{phc::PasswordHash, PasswordHasher, PasswordVerifier},
    Algorithm as Argon2Algorithm, Argon2, Params, Version,
};
use axum::{
    extract::State,
    http::{header::{AUTHORIZATION, RETRY_AFTER}, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use jsonwebtoken::{decode, encode, Algorithm, DecodingKey, EncodingKey, Header, Validation};
use lettre::{
    message::{Mailbox, MultiPart},
    transport::smtp::{
        authentication::Credentials,
        client::{Tls, TlsParameters},
    },
    AsyncSmtpTransport, AsyncTransport, Message, Tokio1Executor,
};
use rand::{rngs::SysRng, TryRng};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};
use sqlx::{any::AnyRow, Row};
use std::{
    time::{SystemTime, UNIX_EPOCH},
};

const ISSUER: &str = "http://0.0.0.0:8080/";
const AUDIENCE: &str = "http://0.0.0.0:8080/resource";
const WRONG_PASSWORD: &str = "Wrong password";
const INVALID_TOKEN: &str = "Invalid or expired token";

type ApiResult = Result<Response, ApiError>;

enum ApiError {
    Status(StatusCode),
    PasswordBusy,
}

impl From<StatusCode> for ApiError {
    fn from(status: StatusCode) -> Self {
        Self::Status(status)
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        match self {
            Self::Status(status) => status.into_response(),
            Self::PasswordBusy => (StatusCode::SERVICE_UNAVAILABLE, [(RETRY_AFTER, "1")], "Password service is busy").into_response(),
        }
    }
}

#[derive(Clone)]
pub enum Mailer {
    Console,
    Smtp {
        from: Mailbox,
        transport: AsyncSmtpTransport<Tokio1Executor>,
    },
}

impl Mailer {
    pub fn new(config: &MailConfig) -> Result<Self, Box<dyn std::error::Error>> {
        match config {
            MailConfig::Console => Ok(Self::Console),
            MailConfig::Smtp(config) => {
                let params = TlsParameters::new(config.host.clone())?;
                let tls = if config.port == 465 {
                    Tls::Wrapper(params)
                } else if !config.username.is_empty() {
                    Tls::Required(params)
                } else {
                    // Unauthenticated local relays such as Mailpit need not offer STARTTLS.
                    Tls::Opportunistic(params)
                };
                let mut builder =
                    AsyncSmtpTransport::<Tokio1Executor>::builder_dangerous(&config.host)
                        .port(config.port)
                        .tls(tls);
                if !config.username.is_empty() {
                    builder = builder.credentials(Credentials::new(
                        config.username.clone(),
                        config.password.clone(),
                    ));
                }
                Ok(Self::Smtp {
                    from: config.from.parse()?,
                    transport: builder.build(),
                })
            }
        }
    }

    async fn send_reset(&self, to: &str, link: &str) -> Result<(), String> {
        let text = format!("You can reset your password at {link}");
        match self {
            Self::Console => {
                println!("Password reset email to {to}: {text}");
                Ok(())
            }
            Self::Smtp { from, transport } => {
                let escaped = html_escape(link);
                let html = format!("<!DOCTYPE html><html><body><p>We received a request to reset your password.</p><p>You can reset your password at <a href=\"{escaped}\">{escaped}</a></p><p>If you didn't make this request, please ignore this email.</p></body></html>");
                let to = to.parse::<Mailbox>().map_err(|e| e.to_string())?;
                let message = Message::builder()
                    .from(from.clone())
                    .to(to)
                    .subject("Password reset")
                    .multipart(MultiPart::alternative_plain_html(text, html))
                    .map_err(|e| e.to_string())?;
                transport.send(message).await.map_err(|e| e.to_string())?;
                Ok(())
            }
        }
    }
}

#[derive(Deserialize)]
struct LoginRequest {
    email: String,
    password: String,
}

#[derive(Deserialize)]
struct ForgotRequest {
    email: String,
}

#[derive(Deserialize)]
struct ResetRequest {
    reset_token: String,
    password: String,
}

#[derive(Serialize, Deserialize)]
struct Claims {
    iss: String,
    aud: String,
    exp: u64,
    user_id: i64,
    version: i64,
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("system time before Unix epoch")
        .as_secs() as i64
}

fn failure(context: &str, error: impl std::fmt::Display) -> StatusCode {
    eprintln!("{context}: {error}");
    StatusCode::INTERNAL_SERVER_ERROR
}

fn decode_user(row: AnyRow) -> Result<(i64, String, i64), sqlx::Error> {
    Ok((
        row.try_get("id")?,
        row.try_get("password_hash")?,
        row.try_get("token_version")?,
    ))
}

async fn find_user(
    state: &AppState,
    email: &str,
) -> Result<Option<(i64, String, i64)>, sqlx::Error> {
    sqlx::query("SELECT id, password_hash, token_version FROM users WHERE email = ?")
        .bind(email)
        .fetch_optional(&state.pool)
        .await?
        .map(decode_user)
        .transpose()
}

async fn hash_password(state: &AppState, password: String) -> Result<String, ApiError> {
    let permit = state.hash_slots.clone().try_acquire_owned().map_err(|_| ApiError::PasswordBusy)?;
    tokio::task::spawn_blocking(move || {
        let _permit = permit;
        let params = Params::new(65536, 3, 4, Some(32)).map_err(|e| e.to_string())?;
        Argon2::new(Argon2Algorithm::Argon2id, Version::V0x13, params)
            .hash_password(password.as_bytes())
            .map(|hash| hash.to_string())
            .map_err(|e| e.to_string())
    })
    .await
    .map_err(|e| ApiError::from(failure("password hash task", e)))?
    .map_err(|e| ApiError::from(failure("password hash", e)))
}

async fn verify_password(
    state: &AppState,
    password: String,
    stored: String,
) -> Result<bool, ApiError> {
    let permit = state.hash_slots.clone().try_acquire_owned().map_err(|_| ApiError::PasswordBusy)?;
    tokio::task::spawn_blocking(move || {
        let _permit = permit;
        PasswordHash::new(&stored).ok().is_some_and(|hash| {
            Argon2::default()
                .verify_password(password.as_bytes(), &hash)
                .is_ok()
        })
    })
    .await
    .map_err(|e| ApiError::from(failure("password verification task", e)))
}

fn password_valid(password: &str) -> bool {
    (2..=24).contains(&password.encode_utf16().count())
}

async fn login(
    State(state): State<AppState>,
    Json(request): Json<LoginRequest>,
) -> ApiResult {
    if !password_valid(&request.password) {
        return Ok((StatusCode::BAD_REQUEST, "400: java.lang.IllegalArgumentException: Password should be from 2 to 24 characters long").into_response());
    }
    let email_length = request.email.encode_utf16().count();
    if !(5..=320).contains(&email_length) || !request.email.contains('@') {
        return Ok((
            StatusCode::BAD_REQUEST,
            "400: java.lang.IllegalArgumentException: Invalid email address",
        )
            .into_response());
    }
    let mut user = find_user(&state, &request.email)
        .await
        .map_err(|e| failure("finding user", e))?;
    let mut newly_created = false;
    if user.is_none() && state.config.allow_new_register {
        let hash = hash_password(&state, request.password.clone()).await?;
        match sqlx::query("INSERT INTO users (email, password_hash) VALUES (?, ?)")
            .bind(&request.email)
            .bind(&hash)
            .execute(&state.pool)
            .await
        {
            Ok(result) => {
                newly_created = true;
                if let Some(id) = result.last_insert_id() {
                    user = Some((id, hash, 0));
                } else {
                    user = find_user(&state, &request.email)
                        .await
                        .map_err(|e| failure("finding created user", e))?;
                }
            }
            Err(error)
                if error
                    .as_database_error()
                    .is_some_and(|e| e.is_unique_violation()) =>
            {
                user = find_user(&state, &request.email)
                    .await
                    .map_err(|e| failure("finding concurrently created user", e))?;
            }
            Err(error) => return Err(failure("creating user", error).into()),
        }
    }
    let Some((id, stored, version)) = user else {
        return Ok((StatusCode::BAD_REQUEST, WRONG_PASSWORD).into_response());
    };
    if !newly_created && !verify_password(&state, request.password, stored).await? {
        return Ok((StatusCode::BAD_REQUEST, WRONG_PASSWORD).into_response());
    }
    let claims = Claims {
        iss: ISSUER.into(),
        aud: AUDIENCE.into(),
        exp: (now() + 30 * 24 * 60 * 60) as u64,
        user_id: id,
        version,
    };
    let token = encode(
        &Header::new(Algorithm::HS256),
        &claims,
        &EncodingKey::from_secret(state.config.jwt_secret.as_bytes())
            .map_err(|e| failure("signing token", e))?,
    )
    .map_err(|e| failure("signing token", e))?;
    Ok(Json(json!({ "token": token })).into_response())
}

pub async fn current_user(headers: &HeaderMap, state: &AppState) -> Result<i64, StatusCode> {
    let authorization = headers
        .get(AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .ok_or(StatusCode::UNAUTHORIZED)?;
    let (scheme, token) = authorization
        .split_once(' ')
        .ok_or(StatusCode::UNAUTHORIZED)?;
    if !scheme.eq_ignore_ascii_case("Bearer") || token.is_empty() {
        return Err(StatusCode::UNAUTHORIZED);
    }
    let mut validation = Validation::new(Algorithm::HS256);
    validation.leeway = 0;
    validation.set_audience(&[AUDIENCE]);
    validation.set_issuer(&[ISSUER]);
    validation.set_required_spec_claims(&["exp", "iss", "aud"]);
    let claims = decode::<Claims>(
        token,
        &DecodingKey::from_secret(state.config.jwt_secret.as_bytes())
            .map_err(|_| StatusCode::UNAUTHORIZED)?,
        &validation,
    )
    .map_err(|_| StatusCode::UNAUTHORIZED)?
    .claims;
    let row = sqlx::query("SELECT token_version FROM users WHERE id = ?")
        .bind(claims.user_id)
        .fetch_optional(&state.pool)
        .await
        .map_err(|e| failure("reading session", e))?
        .ok_or(StatusCode::UNAUTHORIZED)?;
    let version: i64 = row
        .try_get("token_version")
        .map_err(|e| failure("decoding session", e))?;
    if claims.version != version {
        return Err(StatusCode::UNAUTHORIZED);
    }
    Ok(claims.user_id)
}

async fn me(State(state): State<AppState>, headers: HeaderMap) -> ApiResult {
    let id = current_user(&headers, &state).await?;
    let row = sqlx::query("SELECT email FROM users WHERE id = ?")
        .bind(id)
        .fetch_optional(&state.pool)
        .await
        .map_err(|e| failure("reading profile", e))?
        .ok_or(StatusCode::UNAUTHORIZED)?;
    let email: String = row
        .try_get("email")
        .map_err(|e| failure("decoding profile", e))?;
    Ok(Json(json!({ "id": id, "email": email, "nickname": null })).into_response())
}

async fn forgot_password(
    State(state): State<AppState>,
    Json(request): Json<ForgotRequest>,
) -> ApiResult {
    let mut bytes = [0_u8; 32];
    SysRng
        .try_fill_bytes(&mut bytes)
        .map_err(|e| failure("generating password reset token", e))?;
    let token = URL_SAFE_NO_PAD.encode(bytes);
    let digest = hex::encode(Sha256::digest(token.as_bytes()));
    let now = now();
    let result = sqlx::query("UPDATE users SET reset_hash = ?, reset_expires_at = ? WHERE email = ? AND (reset_hash IS NULL OR reset_expires_at IS NULL OR reset_expires_at <= ?)")
        .bind(&digest).bind(now + 900).bind(&request.email).bind(now).execute(&state.pool).await
        .map_err(|e| failure("creating password reset", e))?;
    if result.rows_affected() == 1 {
        let link = format!(
            "{}/deeplink/reset-password?token={token}",
            state.config.base_url.trim_end_matches('/')
        );
        if let Err(error) = state.mailer.send_reset(&request.email, &link).await {
            eprintln!("sending password reset email: {error}");
            if let Err(error) = sqlx::query(
                "UPDATE users SET reset_hash = NULL, reset_expires_at = NULL WHERE reset_hash = ?",
            )
            .bind(&digest)
            .execute(&state.pool)
            .await
            {
                eprintln!("clearing undelivered reset token: {error}");
            }
        }
    }
    Ok((StatusCode::OK, "A password reset email was sent").into_response())
}

async fn reset_password(
    State(state): State<AppState>,
    Json(request): Json<ResetRequest>,
) -> ApiResult {
    let digest = hex::encode(Sha256::digest(request.reset_token.as_bytes()));
    let row = sqlx::query("SELECT id FROM users WHERE reset_hash = ? AND reset_expires_at >= ?")
        .bind(&digest)
        .bind(now())
        .fetch_optional(&state.pool)
        .await
        .map_err(|e| failure("finding password reset", e))?;
    if row.is_none() {
        return Ok((StatusCode::BAD_REQUEST, INVALID_TOKEN).into_response());
    }
    if !password_valid(&request.password) {
        return Ok((
            StatusCode::BAD_REQUEST,
            "Password should be from 2 to 24 characters long",
        )
            .into_response());
    }
    let hash = hash_password(&state, request.password).await?;
    let result = sqlx::query("UPDATE users SET password_hash = ?, reset_hash = NULL, reset_expires_at = NULL, token_version = token_version + 1 WHERE reset_hash = ? AND reset_expires_at >= ?")
        .bind(hash).bind(digest).bind(now()).execute(&state.pool).await
        .map_err(|e| failure("changing password", e))?;
    if result.rows_affected() != 1 {
        return Ok((StatusCode::BAD_REQUEST, INVALID_TOKEN).into_response());
    }
    Ok((StatusCode::OK, "Password has been reset successfully").into_response())
}

pub fn routes() -> Router<AppState> {
    Router::new()
        .route("/auth", post(login))
        .route("/me", get(me))
        .route("/forgot-password", post(forgot_password))
        .route("/reset-password", post(reset_password))
}
