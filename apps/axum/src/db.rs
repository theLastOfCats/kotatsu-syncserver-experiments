use sqlx::{any::AnyPoolOptions, sqlite::SqliteConnectOptions, AnyPool, ConnectOptions};
use std::{env, io, time::Duration};
use url::Url;

pub async fn connect() -> Result<(AnyPool, String), Box<dyn std::error::Error>> {
    sqlx::any::install_default_drivers();
    let dialect = env::var("DATABASE_DIALECT").unwrap_or_else(|_| "sqlite".into());
    let mut options = AnyPoolOptions::new()
        .acquire_timeout(Duration::from_secs(60))
        .max_lifetime(None)
        .idle_timeout(None);
    let database_url = match dialect.as_str() {
        "sqlite" => {
            let path = env::var("DATABASE_PATH").unwrap_or_else(|_| "kotatsu.sqlite".into());
            options = options.max_connections(1).after_connect(|conn, _| {
                Box::pin(async move {
                    sqlx::query("PRAGMA journal_mode = WAL")
                        .execute(&mut *conn)
                        .await?;
                    sqlx::query("PRAGMA synchronous = FULL")
                        .execute(&mut *conn)
                        .await?;
                    sqlx::query("PRAGMA busy_timeout = 30000")
                        .execute(&mut *conn)
                        .await?;
                    sqlx::query("PRAGMA foreign_keys = ON")
                        .execute(&mut *conn)
                        .await?;
                    Ok(())
                })
            });
            if path == ":memory:" {
                "sqlite::memory:".to_owned()
            } else {
                SqliteConnectOptions::new()
                    .filename(path)
                    .create_if_missing(true)
                    .to_url_lossy()
                    .to_string()
            }
        }
        "mysql" => {
            options = options.max_connections(16);
            let host = env::var("DATABASE_HOST").unwrap_or_else(|_| "localhost".into());
            let port = env::var("DATABASE_PORT")
                .unwrap_or_else(|_| "3306".into())
                .parse::<u16>()?;
            let user = env::var("DATABASE_USER").unwrap_or_else(|_| "kotatsu".into());
            let password = env::var("DATABASE_PASSWORD").unwrap_or_default();
            let name = env::var("DATABASE_NAME").unwrap_or_else(|_| "kotatsu_db".into());
            let mut url = Url::parse("mysql://localhost/")?;
            url.set_host(Some(&host))
                .map_err(|_| io::Error::other("invalid DATABASE_HOST"))?;
            url.set_port(Some(port))
                .map_err(|_| io::Error::other("invalid DATABASE_PORT"))?;
            url.set_username(&user)
                .map_err(|_| io::Error::other("invalid DATABASE_USER"))?;
            url.set_password(Some(&password))
                .map_err(|_| io::Error::other("invalid DATABASE_PASSWORD"))?;
            url.set_path(&name);
            url.to_string()
        }
        _ => {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "DATABASE_DIALECT must be sqlite or mysql",
            )
            .into())
        }
    };
    let pool = options.connect(&database_url).await?;
    Ok((pool, dialect))
}

pub async fn migrate(pool: &AnyPool, dialect: &str) -> Result<(), sqlx::Error> {
    let users = if dialect == "mysql" {
        "CREATE TABLE IF NOT EXISTS users (\
            id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\
            email VARCHAR(320) NOT NULL UNIQUE,\
            password_hash VARCHAR(255) NOT NULL,\
            reset_hash VARCHAR(64),\
            reset_expires_at BIGINT,\
            token_version BIGINT NOT NULL DEFAULT 0,\
            INDEX users_reset_hash (reset_hash)\
        ) ENGINE=InnoDB"
    } else {
        "CREATE TABLE IF NOT EXISTS users (\
            id INTEGER PRIMARY KEY AUTOINCREMENT,\
            email TEXT NOT NULL UNIQUE,\
            password_hash TEXT NOT NULL,\
            reset_hash TEXT,\
            reset_expires_at INTEGER,\
            token_version INTEGER NOT NULL DEFAULT 0\
        )"
    };
    sqlx::query(users).execute(pool).await?;
    if dialect == "sqlite" {
        sqlx::query("CREATE INDEX IF NOT EXISTS users_reset_hash ON users(reset_hash)")
            .execute(pool)
            .await?;
    }
    Ok(())
}
