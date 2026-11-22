package kotatsu

import com.auth0.jwt.JWT
import com.auth0.jwt.algorithms.Algorithm
import com.auth0.jwt.exceptions.JWTVerificationException
import de.mkammerer.argon2.Argon2Factory
import io.ktor.http.ContentType
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.server.application.ApplicationCall
import io.ktor.server.plugins.ratelimit.RateLimitName
import io.ktor.server.plugins.ratelimit.rateLimit
import io.ktor.server.request.header
import io.ktor.server.request.receive
import io.ktor.server.response.respond
import io.ktor.server.response.respondText
import io.ktor.server.routing.Route
import io.ktor.server.routing.get
import io.ktor.server.routing.post
import jakarta.mail.Authenticator
import jakarta.mail.Message
import jakarta.mail.PasswordAuthentication
import jakarta.mail.Session
import jakarta.mail.Transport
import jakarta.mail.internet.InternetAddress
import jakarta.mail.internet.MimeBodyPart
import jakarta.mail.internet.MimeMessage
import jakarta.mail.internet.MimeMultipart
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Semaphore
import kotlinx.coroutines.sync.withPermit
import kotlinx.coroutines.withContext
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import java.net.URLEncoder
import java.security.MessageDigest
import java.security.SecureRandom
import java.sql.Connection
import java.time.Instant
import java.util.Base64
import java.util.Date
import java.util.HexFormat
import java.util.Properties
import java.util.concurrent.TimeUnit

private const val JWT_ISSUER = "http://0.0.0.0:8080/"
private const val JWT_AUDIENCE = "http://0.0.0.0:8080/resource"
private const val RESET_LIFETIME_SECONDS = 15 * 60L
private const val RESET_INVALID = "Invalid or expired token"
private const val PASSWORD_INVALID = "Password should be from 2 to 24 characters long"
private const val FORGOT_RESPONSE = "A password reset email was sent"

@Serializable
private data class AuthRequest(val email: String, val password: String)

@Serializable
private data class AuthResponse(val token: String)

@Serializable
private data class ForgotPasswordRequest(val email: String)

@Serializable
private data class ResetPasswordRequest(@SerialName("reset_token") val resetToken: String, val password: String)

@Serializable
private data class UserInfo(val id: Long, val email: String, val nickname: String?)

private data class Credentials(val id: Long, val passwordHash: String)
private data class AuthSession(val userId: Long, val version: Long)
private data class RecoveryRecipient(val id: Long, val email: String)

private val secureRandom = SecureRandom()
private val argon2 = Argon2Factory.create(Argon2Factory.Argon2Types.ARGON2id)
// Bound simultaneous 64 MiB password operations independently of request concurrency.
private val passwordPermits = Semaphore(2)

private class JwtMaterial(secret: String) {
    val algorithm = Algorithm.HMAC256(secret)
    val verifier = JWT.require(algorithm).withIssuer(JWT_ISSUER).withAudience(JWT_AUDIENCE).build()
}

private val jwtLock = Any()
@Volatile private var cachedJwt: Pair<String, JwtMaterial>? = null

private fun jwtMaterial(secret: String): JwtMaterial {
    cachedJwt?.let { if (it.first == secret) return it.second }
    return synchronized(jwtLock) {
        cachedJwt?.let { if (it.first == secret) return@synchronized it.second }
        JwtMaterial(secret).also { cachedJwt = secret to it }
    }
}

private suspend fun hashPassword(password: String): String = passwordPermits.withPermit {
    withContext(Dispatchers.Default) {
        val chars = password.toCharArray()
        try {
            argon2.hash(3, 65_536, 4, chars)
        } finally {
            chars.fill('\u0000')
        }
    }
}

private suspend fun verifyPassword(password: String, hash: String): Boolean = passwordPermits.withPermit {
    withContext(Dispatchers.Default) {
        val chars = password.toCharArray()
        try {
            argon2.verify(hash, chars)
        } finally {
            chars.fill('\u0000')
        }
    }
}

private suspend fun <T> authTransaction(
    db: Database,
    userId: Long? = null,
    block: (Connection) -> T,
): T = withContext(Dispatchers.IO) { db.transaction(userId, block) }

fun initAuth(db: Database) {
    val sql = if (db.dialect == "sqlite") {
        """
        CREATE TABLE IF NOT EXISTS users (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            email VARCHAR(320) NOT NULL UNIQUE,
            password_hash VARCHAR(255) NOT NULL,
            session_version BIGINT NOT NULL DEFAULT 0,
            nickname VARCHAR(100),
            password_reset_token_hash CHAR(64) UNIQUE,
            password_reset_token_expires_at BIGINT
        )
        """.trimIndent()
    } else {
        """
        CREATE TABLE IF NOT EXISTS users (
            id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
            email VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL UNIQUE,
            password_hash VARCHAR(255) NOT NULL,
            session_version BIGINT NOT NULL DEFAULT 0,
            nickname VARCHAR(100),
            password_reset_token_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin UNIQUE,
            password_reset_token_expires_at BIGINT
        ) ENGINE=InnoDB
        """.trimIndent()
    }
    db.transaction { connection -> connection.createStatement().use { it.execute(sql) } }
}

private fun findCredentials(connection: Connection, email: String): Credentials? =
    connection.prepareStatement("SELECT id, password_hash FROM users WHERE email = ?").use { statement ->
        statement.setString(1, email)
        statement.executeQuery().use { rows ->
            if (rows.next()) Credentials(rows.getLong(1), rows.getString(2)) else null
        }
    }

private suspend fun authenticate(db: Database, config: Config, request: AuthRequest): AuthSession? {
    var credentials = authTransaction(db) { findCredentials(it, request.email) }
    var generatedHash: String? = null
    if (credentials == null && config.allowNewRegister) {
        val hash = hashPassword(request.password)
        generatedHash = hash
        credentials = authTransaction(db) { connection ->
            // The unique email constraint decides concurrent registrations without overwriting a password.
            val sql = if (db.dialect == "sqlite") {
                "INSERT INTO users (email, password_hash) VALUES (?, ?) ON CONFLICT(email) DO NOTHING"
            } else {
                "INSERT INTO users (email, password_hash) VALUES (?, ?) ON DUPLICATE KEY UPDATE id = id"
            }
            connection.prepareStatement(sql).use { statement ->
                statement.setString(1, request.email)
                statement.setString(2, hash)
                statement.executeUpdate()
            }
            findCredentials(connection, request.email)
        }
    }
    val user = credentials ?: return null
    if (generatedHash != user.passwordHash && !verifyPassword(request.password, user.passwordHash)) return null

    // A reset may have won while Argon2 was running. Do not authenticate an obsolete hash.
    return authTransaction(db, user.id) { connection ->
        connection.prepareStatement("SELECT password_hash, session_version FROM users WHERE id = ?").use { statement ->
            statement.setLong(1, user.id)
            statement.executeQuery().use { rows ->
                if (rows.next() && rows.getString(1) == user.passwordHash) {
                    AuthSession(user.id, rows.getLong(2))
                } else {
                    null
                }
            }
        }
    }
}

private fun issueToken(session: AuthSession, secret: String): String = JWT.create()
    .withIssuer(JWT_ISSUER)
    .withAudience(JWT_AUDIENCE)
    .withClaim("user_id", session.userId)
    .withClaim("session_version", session.version)
    .withExpiresAt(Date(System.currentTimeMillis() + TimeUnit.DAYS.toMillis(30)))
    .sign(jwtMaterial(secret).algorithm)

private fun verifiedSession(token: String, secret: String): AuthSession? {
    return try {
        val decoded = jwtMaterial(secret).verifier.verify(token)
        val expiresAt = decoded.expiresAt ?: return null
        if (expiresAt.time <= System.currentTimeMillis()) return null
        val userId = decoded.getClaim("user_id").asLong()?.takeIf { it > 0 } ?: return null
        val version = decoded.getClaim("session_version").asLong()?.takeIf { it >= 0 } ?: return null
        AuthSession(userId, version)
    } catch (_: JWTVerificationException) {
        null
    } catch (_: IllegalArgumentException) {
        null
    }
}

suspend fun ApplicationCall.authenticatedUserId(db: Database, config: Config): Long? {
    val header = request.header(HttpHeaders.Authorization)
    val bearer = header?.takeIf { it.startsWith("Bearer ", ignoreCase = true) }
        ?.substringAfter(' ')?.trim()?.takeIf { it.isNotEmpty() }
    val session = bearer?.let { verifiedSession(it, config.jwtSecret) }
    if (session != null) {
        val exists = authTransaction(db, session.userId) { connection ->
            connection.prepareStatement("SELECT 1 FROM users WHERE id = ? AND session_version = ?").use { statement ->
                statement.setLong(1, session.userId)
                statement.setLong(2, session.version)
                statement.executeQuery().use { it.next() }
            }
        }
        if (exists) return session.userId
    }
    respond(HttpStatusCode.Unauthorized)
    return null
}

private fun tokenHash(token: String): String =
    HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(token.toByteArray(Charsets.UTF_8)))

private fun newResetToken(): String {
    val bytes = ByteArray(32)
    secureRandom.nextBytes(bytes)
    return Base64.getUrlEncoder().withoutPadding().encodeToString(bytes)
}

private fun queryComponent(value: String): String = URLEncoder.encode(value, Charsets.UTF_8).replace("+", "%20")

private fun htmlEscape(value: String): String = buildString(value.length) {
    for (character in value) {
        when (character) {
            '&' -> append("&amp;")
            '<' -> append("&lt;")
            '>' -> append("&gt;")
            '"' -> append("&quot;")
            '\'' -> append("&#39;")
            else -> append(character)
        }
    }
}

private class RecoveryMailer(private val config: Config) {
    private val smtpSession: Session? = if (config.mailProvider == "smtp") {
        val authenticated = config.smtpUsername.isNotBlank()
        val properties = Properties().apply {
            put("mail.smtp.host", config.smtpHost)
            put("mail.smtp.port", config.smtpPort.toString())
            put("mail.smtp.auth", authenticated.toString())
            put("mail.smtp.connectiontimeout", "10000")
            put("mail.smtp.timeout", "10000")
            put("mail.smtp.writetimeout", "10000")
            if (config.smtpPort == 465) put("mail.smtp.ssl.enable", "true")
            else put("mail.smtp.starttls.enable", "true")
        }
        if (authenticated) {
            Session.getInstance(properties, object : Authenticator() {
                override fun getPasswordAuthentication(): PasswordAuthentication =
                    PasswordAuthentication(config.smtpUsername, config.smtpPassword)
            })
        } else {
            Session.getInstance(properties)
        }
    } else null

    fun send(to: String, link: String) {
        val text = "You can reset your password at $link"
        if (smtpSession == null) {
            println("=== ConsoleMail ===\nTo: $to\nSubject: Password reset\nBody (plain):\n$text\n==================")
            return
        }
        val escapedLink = htmlEscape(link)
        val html = """
            <!DOCTYPE html>
            <html lang="en">
            <head>
                <meta charset="UTF-8">
                <title>Forgot password</title>
            </head>
            <body style="font-family:-apple-system,'Segoe UI',Roboto,Arial,sans-serif;">
            <main>
                <p>We received a request to reset your password.</p>
                <p>You can reset your password at <a href="$escapedLink">$escapedLink</a></p>
                <p>If you didn't make this request, please ignore this email.</p>
            </main>
            </body>
            </html>
        """.trimIndent()
        val parts = MimeMultipart("alternative")
        parts.addBodyPart(MimeBodyPart().apply { setText(text, "UTF-8") })
        parts.addBodyPart(MimeBodyPart().apply { setContent(html, "text/html; charset=UTF-8") })
        val message = MimeMessage(smtpSession).apply {
            setFrom(InternetAddress(config.smtpFrom, true))
            setRecipient(Message.RecipientType.TO, InternetAddress(to, true))
            setSubject("Password reset", "UTF-8")
            setContent(parts)
        }
        Transport.send(message)
    }
}

private suspend fun sendRecovery(db: Database, config: Config, mailer: RecoveryMailer, email: String) {
    val recipient = authTransaction(db) { connection ->
        connection.prepareStatement("SELECT id, email FROM users WHERE email = ?").use { statement ->
            statement.setString(1, email)
            statement.executeQuery().use { rows ->
                if (rows.next()) RecoveryRecipient(rows.getLong(1), rows.getString(2)) else null
            }
        }
    } ?: return

    val token = newResetToken()
    val hash = tokenHash(token)
    val now = Instant.now().epochSecond
    val claimed = authTransaction(db, recipient.id) { connection ->
        connection.prepareStatement(
            """UPDATE users SET password_reset_token_hash = ?, password_reset_token_expires_at = ?
               WHERE id = ? AND (password_reset_token_hash IS NULL OR password_reset_token_expires_at IS NULL
                   OR password_reset_token_expires_at <= ?)""".trimIndent(),
        ).use { statement ->
            statement.setString(1, hash)
            statement.setLong(2, now + RESET_LIFETIME_SECONDS)
            statement.setLong(3, recipient.id)
            statement.setLong(4, now)
            statement.executeUpdate() == 1
        }
    }
    if (!claimed) return

    val link = "${config.baseUrl.trimEnd('/')}/deeplink/reset-password?token=${queryComponent(token)}"
    try {
        withContext(Dispatchers.IO) { mailer.send(recipient.email, link) }
    } catch (e: CancellationException) {
        throw e
    } catch (_: Exception) {
        // Permit a later request to retry delivery without clearing a newer token.
        authTransaction(db, recipient.id) { connection ->
            connection.prepareStatement(
                """UPDATE users SET password_reset_token_hash = NULL, password_reset_token_expires_at = NULL
                   WHERE id = ? AND password_reset_token_hash = ?""".trimIndent(),
            ).use { statement ->
                statement.setLong(1, recipient.id)
                statement.setString(2, hash)
                statement.executeUpdate()
            }
        }
        System.err.println("Password reset email delivery failed")
    }
}

private suspend fun findResetUser(db: Database, hash: String): Long? = authTransaction(db) { connection ->
    connection.prepareStatement(
        "SELECT id FROM users WHERE password_reset_token_hash = ? AND password_reset_token_expires_at >= ?",
    ).use { statement ->
        statement.setString(1, hash)
        statement.setLong(2, Instant.now().epochSecond)
        statement.executeQuery().use { rows -> if (rows.next()) rows.getLong(1) else null }
    }
}

private suspend fun consumeReset(db: Database, userId: Long, hash: String, passwordHash: String): Boolean =
    authTransaction(db, userId) { connection ->
        connection.prepareStatement(
            """UPDATE users SET password_hash = ?, session_version = session_version + 1,
                   password_reset_token_hash = NULL, password_reset_token_expires_at = NULL
               WHERE id = ? AND password_reset_token_hash = ? AND password_reset_token_expires_at >= ?""".trimIndent(),
        ).use { statement ->
            statement.setString(1, passwordHash)
            statement.setLong(2, userId)
            statement.setString(3, hash)
            statement.setLong(4, Instant.now().epochSecond)
            statement.executeUpdate() == 1
        }
    }

fun Route.authRoutes(db: Database, config: Config) {
    val mailer = RecoveryMailer(config)

    rateLimit(RateLimitName("auth")) {
        post("/auth") {
            val request = call.receive<AuthRequest>()
            val validationError = when {
                request.password.length !in 2..24 -> PASSWORD_INVALID
                request.email.length !in 5..320 || '@' !in request.email -> "Invalid email address"
                else -> null
            }
            if (validationError != null) {
                call.respondText("400: java.lang.IllegalArgumentException: $validationError", status = HttpStatusCode.BadRequest)
                return@post
            }
            val session = authenticate(db, config, request)
            if (session == null) {
                call.respondText("Wrong password", status = HttpStatusCode.BadRequest)
            } else {
                call.respond(AuthResponse(issueToken(session, config.jwtSecret)))
            }
        }
    }

    rateLimit(RateLimitName("forgotPassword")) {
        post("/forgot-password") {
            val request = call.receive<ForgotPasswordRequest>()
            sendRecovery(db, config, mailer, request.email)
            call.respondText(FORGOT_RESPONSE)
        }
    }

    rateLimit(RateLimitName("resetPassword")) {
        post("/reset-password") {
            val request = call.receive<ResetPasswordRequest>()
            val hash = tokenHash(request.resetToken)
            val userId = findResetUser(db, hash)
            if (userId == null) {
                call.respondText(RESET_INVALID, status = HttpStatusCode.BadRequest)
                return@post
            }
            if (request.password.length !in 2..24) {
                call.respondText(PASSWORD_INVALID, status = HttpStatusCode.BadRequest)
                return@post
            }
            val passwordHash = hashPassword(request.password)
            if (consumeReset(db, userId, hash, passwordHash)) {
                call.respondText("Password has been reset successfully")
            } else {
                call.respondText(RESET_INVALID, status = HttpStatusCode.BadRequest)
            }
        }
    }

    get("/me") {
        val userId = call.authenticatedUserId(db, config) ?: return@get
        val user = authTransaction(db, userId) { connection ->
            connection.prepareStatement("SELECT email, nickname FROM users WHERE id = ?").use { statement ->
                statement.setLong(1, userId)
                statement.executeQuery().use { rows ->
                    if (rows.next()) UserInfo(userId, rows.getString(1), rows.getString(2)) else null
                }
            }
        }
        if (user == null) call.respond(HttpStatusCode.Unauthorized) else call.respond(user)
    }

    get("/deeplink/reset-password") {
        val token = call.request.queryParameters["token"]
        if (token.isNullOrBlank()) {
            call.respondText("Missing token", status = HttpStatusCode.BadRequest)
            return@get
        }
        // Both components are percent-encoded before embedding the URI in JavaScript and HTML.
        val deepLink = "kotatsu://reset-password?base_url=${queryComponent(config.baseUrl)}&token=${queryComponent(token)}"
        val html = """
            <!DOCTYPE html>
            <html lang="en">
            <head>
                <meta charset="UTF-8">
                <title>Reset password</title>
                <style>
                    body {
                        font-family:-apple-system,'Segoe UI',Roboto,Arial,sans-serif;
                    }
                </style>
                <script type="text/javascript">
                    window.location.href = "$deepLink";
                </script>
            </head>
            <body>
            <main>
                <p>If you are not redirected automatically, <a href="${htmlEscape(deepLink)}">click here</a>.</p>
            </main>
            </body>
            </html>
        """.trimIndent() + "\n"
        call.respondText(html, ContentType.Text.Html)
    }
}
