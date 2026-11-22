use std::{env, io};

#[derive(Clone)]
pub struct SmtpConfig {
    pub host: String,
    pub port: u16,
    pub username: String,
    pub password: String,
    pub from: String,
}

#[derive(Clone)]
pub enum MailConfig {
    Console,
    Smtp(SmtpConfig),
}

pub struct Config {
    pub port: u16,
    pub jwt_secret: String,
    pub base_url: String,
    pub allow_new_register: bool,
    pub auth_rate_limit: usize,
    pub mail: MailConfig,
}

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}

impl Config {
    pub fn from_env() -> Result<Self, io::Error> {
        let port = env::var("PORT")
            .unwrap_or_else(|_| "8080".into())
            .parse()
            .map_err(|_| invalid("invalid PORT"))?;
        let jwt_secret = env::var("JWT_SECRET").map_err(|_| invalid("JWT_SECRET is required"))?;
        if jwt_secret.is_empty() {
            return Err(invalid("JWT_SECRET must not be empty"));
        }
        let base_url = env::var("BASE_URL").unwrap_or_else(|_| format!("http://127.0.0.1:{port}"));
        let allow_new_register = match env::var("ALLOW_NEW_REGISTER").as_deref() {
            Ok("true") | Err(_) => true,
            Ok("false") => false,
            _ => return Err(invalid("ALLOW_NEW_REGISTER must be true or false")),
        };
        let auth_rate_limit = env::var("AUTH_RATE_LIMIT")
            .unwrap_or_else(|_| "5".into())
            .parse::<usize>()
            .map_err(|_| invalid("invalid AUTH_RATE_LIMIT"))?;
        if auth_rate_limit == 0 {
            return Err(invalid("AUTH_RATE_LIMIT must be positive"));
        }
        let mail = match env::var("MAIL_PROVIDER")
            .unwrap_or_else(|_| "console".into())
            .as_str()
        {
            "console" => MailConfig::Console,
            "smtp" => {
                let host = env::var("SMTP_HOST").map_err(|_| invalid("SMTP_HOST is required"))?;
                let from = env::var("SMTP_FROM").map_err(|_| invalid("SMTP_FROM is required"))?;
                if host.is_empty() || from.is_empty() {
                    return Err(invalid("SMTP_HOST and SMTP_FROM must not be empty"));
                }
                let port = env::var("SMTP_PORT")
                    .unwrap_or_else(|_| "25".into())
                    .parse()
                    .map_err(|_| invalid("invalid SMTP_PORT"))?;
                MailConfig::Smtp(SmtpConfig {
                    host,
                    port,
                    username: env::var("SMTP_USERNAME").unwrap_or_default(),
                    password: env::var("SMTP_PASSWORD").unwrap_or_default(),
                    from,
                })
            }
            _ => return Err(invalid("MAIL_PROVIDER must be console or smtp")),
        };
        Ok(Self {
            port,
            jwt_secret,
            base_url,
            allow_new_register,
            auth_rate_limit,
            mail,
        })
    }
}
