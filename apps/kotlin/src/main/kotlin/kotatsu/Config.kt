package kotatsu

import java.net.URI

class Config private constructor(
    val dialect: String,
    val port: Int,
    val bindHost: String,
    val databasePath: String,
    val databaseHost: String,
    val databasePort: Int,
    val databaseName: String,
    val databaseUser: String,
    val databasePassword: String,
    val jwtSecret: String,
    val baseUrl: String,
    val allowNewRegister: Boolean,
    val mailProvider: String,
    val smtpHost: String,
    val smtpPort: Int,
    val smtpUsername: String,
    val smtpPassword: String,
    val smtpFrom: String,
    val apiRateLimit: Int,
    val authRateLimit: Int,
    val forgotRateLimit: Int,
    val resetRateLimit: Int,
    val passwordConcurrency: Int,
) {
    companion object {
        fun fromEnvironment(env: Map<String, String> = System.getenv()): Config {
            fun value(name: String, default: String): String = env[name]?.takeIf { it.isNotBlank() } ?: default
            fun port(name: String, default: Int): Int {
                val raw = value(name, default.toString())
                return raw.toIntOrNull()?.takeIf { it in 1..65535 }
                    ?: throw IllegalArgumentException("$name must be a valid TCP port")
            }
            val dialect = value("DATABASE_DIALECT", "sqlite").lowercase()
            require(dialect == "sqlite" || dialect == "mysql") { "DATABASE_DIALECT must be sqlite or mysql" }
            val serverPort = port("PORT", 8080)
            val baseUrl = value("BASE_URL", "http://127.0.0.1:$serverPort")
            val baseUri = try {
                URI(baseUrl)
            } catch (e: Exception) {
                throw IllegalArgumentException("BASE_URL must be an absolute HTTP URL", e)
            }
            require(baseUri.scheme in setOf("http", "https") && baseUri.host != null &&
                baseUri.userInfo == null && baseUri.fragment == null && baseUri.query == null
            ) { "BASE_URL must be an absolute HTTP URL without credentials, query, or fragment" }
            val allowNewRegister = value("ALLOW_NEW_REGISTER", "true").toBooleanStrictOrNull()
                ?: throw IllegalArgumentException("ALLOW_NEW_REGISTER must be true or false")
            val mailProvider = value("MAIL_PROVIDER", "console").lowercase()
            require(mailProvider == "console" || mailProvider == "smtp") { "MAIL_PROVIDER must be console or smtp" }
            val secret = env["JWT_SECRET"]?.takeIf { it.isNotBlank() }
                ?: throw IllegalArgumentException("JWT_SECRET must be set")
            fun positiveInt(name: String, default: Int): Int =
                (env[name] ?: default.toString()).toIntOrNull()?.takeIf { it > 0 }
                    ?: throw IllegalArgumentException("$name must be a positive integer")
            return Config(
                dialect = dialect,
                port = serverPort,
                bindHost = value("BIND_HOST", "127.0.0.1"),
                databasePath = value("DATABASE_PATH", "kotatsu.db"),
                databaseHost = value("DATABASE_HOST", "127.0.0.1"),
                databasePort = port("DATABASE_PORT", 3306),
                databaseName = value("DATABASE_NAME", "kotatsu_db"),
                databaseUser = value("DATABASE_USER", "kotatsu"),
                databasePassword = env["DATABASE_PASSWORD"] ?: "",
                jwtSecret = secret,
                baseUrl = baseUrl,
                allowNewRegister = allowNewRegister,
                mailProvider = mailProvider,
                smtpHost = value("SMTP_HOST", "127.0.0.1"),
                smtpPort = port("SMTP_PORT", 25),
                smtpUsername = env["SMTP_USERNAME"] ?: "",
                smtpPassword = env["SMTP_PASSWORD"] ?: "",
                smtpFrom = value("SMTP_FROM", "no-reply@localhost"),
                apiRateLimit = positiveInt("RATE_LIMIT_API_PER_SECOND", 200),
                authRateLimit = positiveInt("RATE_LIMIT_AUTH_PER_MINUTE", 5),
                forgotRateLimit = positiveInt("RATE_LIMIT_FORGOT_PER_10_MINUTES", 5),
                resetRateLimit = positiveInt("RATE_LIMIT_RESET_PER_10_MINUTES", 5),
                passwordConcurrency = positiveInt("RATE_LIMIT_PASSWORD_CONCURRENCY", 2),
            )
        }
    }
}
