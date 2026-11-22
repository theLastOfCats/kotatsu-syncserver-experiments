using System.Data.Common;
using System.Globalization;
using System.Security.Cryptography;
using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Text.Json.Serialization.Metadata;
using Konscious.Security.Cryptography;
using MailKit.Security;
using Microsoft.AspNetCore.Http.Features;
using MimeKit;

public static class Auth
{
    private const int PasswordMemoryKiB = 65_536;
    private const int PasswordIterations = 3;
    private const int PasswordParallelism = 4;
    private const string PasswordPrefix = "$argon2id$v=19$m=65536,t=3,p=4$";
    private const string JwtHeader = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9";
    private static readonly AuthJsonContext RequestJson = new(new JsonSerializerOptions(JsonSerializerDefaults.Web)
    {
        PropertyNameCaseInsensitive = false
    });
    private static readonly AuthJsonContext ResponseJson = new(new Microsoft.AspNetCore.Http.Json.JsonOptions().SerializerOptions);
    private static byte[] jwtSecret = Array.Empty<byte>();
    private static string issuer = "http://0.0.0.0:8080/";
    private static string audience = "http://0.0.0.0:8080/resource";
    private static string baseUrl = "http://localhost:8080";
    private static bool allowRegistration = true;
    private static string mailProvider = "console";
    private static string smtpHost = "";
    private static int smtpPort;
    private static string smtpUsername = "";
    private static string smtpPassword = "";
    private static MailboxAddress? smtpFrom;
    private static SecureSocketOptions smtpSecurity;

    public static void Initialize()
    {
        var secret = Environment.GetEnvironmentVariable("JWT_SECRET");
        if (string.IsNullOrWhiteSpace(secret))
            throw new InvalidOperationException("JWT_SECRET must be configured");
        jwtSecret = Encoding.UTF8.GetBytes(secret);
        issuer = Environment.GetEnvironmentVariable("JWT_ISSUER") ?? issuer;
        audience = Environment.GetEnvironmentVariable("JWT_AUDIENCE") ?? audience;

        var registration = Environment.GetEnvironmentVariable("ALLOW_NEW_REGISTER");
        if (!string.IsNullOrEmpty(registration) && !bool.TryParse(registration, out allowRegistration))
            throw new InvalidOperationException("ALLOW_NEW_REGISTER must be true or false");

        var configuredBaseUrl = Environment.GetEnvironmentVariable("BASE_URL")
            ?? $"http://localhost:{Environment.GetEnvironmentVariable("PORT") ?? "8080"}";
        if (!Uri.TryCreate(configuredBaseUrl, UriKind.Absolute, out var uri)
            || (uri.Scheme != Uri.UriSchemeHttp && uri.Scheme != Uri.UriSchemeHttps)
            || string.IsNullOrEmpty(uri.Host) || !string.IsNullOrEmpty(uri.UserInfo)
            || !string.IsNullOrEmpty(uri.Query) || !string.IsNullOrEmpty(uri.Fragment))
            throw new InvalidOperationException("BASE_URL must be an absolute HTTP(S) URL without credentials, query, or fragment");
        baseUrl = uri.AbsoluteUri.TrimEnd('/');

        mailProvider = (Environment.GetEnvironmentVariable("MAIL_PROVIDER") ?? "console").ToLowerInvariant();
        if (mailProvider == "smtp")
        {
            smtpHost = Environment.GetEnvironmentVariable("SMTP_HOST") ?? "";
            var port = Environment.GetEnvironmentVariable("SMTP_PORT") ?? "587";
            if (string.IsNullOrWhiteSpace(smtpHost)
                || !int.TryParse(port, NumberStyles.None, CultureInfo.InvariantCulture, out smtpPort)
                || smtpPort is < 1 or > 65535)
                throw new InvalidOperationException("SMTP_HOST and a valid SMTP_PORT are required");
            var from = Environment.GetEnvironmentVariable("SMTP_FROM");
            if (string.IsNullOrWhiteSpace(from) || !MailboxAddress.TryParse(from, out smtpFrom))
                throw new InvalidOperationException("SMTP_FROM must be a mailbox address");
            smtpUsername = Environment.GetEnvironmentVariable("SMTP_USERNAME") ?? "";
            smtpPassword = Environment.GetEnvironmentVariable("SMTP_PASSWORD") ?? "";
            if ((smtpUsername.Length == 0) != (smtpPassword.Length == 0))
                throw new InvalidOperationException("SMTP_USERNAME and SMTP_PASSWORD must be provided together");
            var security = (Environment.GetEnvironmentVariable("SMTP_SECURITY") ?? "auto").ToLowerInvariant();
            smtpSecurity = security switch
            {
                "auto" when smtpPort == 465 => SecureSocketOptions.SslOnConnect,
                "auto" when smtpUsername.Length != 0 => SecureSocketOptions.StartTls,
                "auto" => SecureSocketOptions.StartTlsWhenAvailable,
                "starttls" => SecureSocketOptions.StartTls,
                "ssl" => SecureSocketOptions.SslOnConnect,
                "none" when smtpUsername.Length == 0 => SecureSocketOptions.None,
                _ => throw new InvalidOperationException("SMTP_SECURITY must be auto, starttls, ssl, or none (without credentials)")
            };
        }
        else if (mailProvider != "console")
        {
            throw new InvalidOperationException("MAIL_PROVIDER must be console or smtp");
        }

        using var connection = Database.Open();
        using (var command = connection.CreateCommand())
        {
            command.CommandText = Database.Dialect == "sqlite"
                ? """
                  CREATE TABLE IF NOT EXISTS users (
                      id INTEGER PRIMARY KEY AUTOINCREMENT,
                      email TEXT NOT NULL UNIQUE,
                      password_hash TEXT NOT NULL,
                      nickname TEXT NULL,
                      password_reset_token_hash TEXT NULL,
                      password_reset_token_expires_at INTEGER NULL,
                      session_version INTEGER NOT NULL DEFAULT 0
                  )
                  """
                : """
                  CREATE TABLE IF NOT EXISTS users (
                      id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
                      email VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
                      password_hash VARCHAR(255) NOT NULL,
                      nickname VARCHAR(100) NULL,
                      password_reset_token_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
                      password_reset_token_expires_at BIGINT NULL,
                      session_version BIGINT NOT NULL DEFAULT 0,
                      UNIQUE KEY uq_users_email (email)
                  ) ENGINE=InnoDB
                  """;
            command.ExecuteNonQuery();
        }

        var columns = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
        using (var command = connection.CreateCommand())
        {
            command.CommandText = Database.Dialect == "sqlite"
                ? "PRAGMA table_info(users)"
                : "SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'users'";
            using var reader = command.ExecuteReader();
            while (reader.Read()) columns.Add(reader.GetString(Database.Dialect == "sqlite" ? 1 : 0));
        }
        AddColumn(connection, columns, "nickname", Database.Dialect == "sqlite" ? "TEXT NULL" : "VARCHAR(100) NULL");
        AddColumn(connection, columns, "password_reset_token_hash", Database.Dialect == "sqlite" ? "TEXT NULL" : "CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL");
        AddColumn(connection, columns, "password_reset_token_expires_at", "BIGINT NULL");
        AddColumn(connection, columns, "session_version", "BIGINT NOT NULL DEFAULT 0");

        using (var command = connection.CreateCommand())
        {
            if (Database.Dialect == "sqlite")
            {
                command.CommandText = "CREATE UNIQUE INDEX IF NOT EXISTS uq_users_password_reset_token_hash ON users (password_reset_token_hash)";
                command.ExecuteNonQuery();
            }
            else
            {
                command.CommandText = "SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'users' AND INDEX_NAME = 'uq_users_password_reset_token_hash'";
                if (Convert.ToInt64(command.ExecuteScalar(), CultureInfo.InvariantCulture) == 0)
                {
                    command.CommandText = "CREATE UNIQUE INDEX uq_users_password_reset_token_hash ON users (password_reset_token_hash)";
                    command.ExecuteNonQuery();
                }
            }
        }
    }

    public static void Map(WebApplication app)
    {
        app.MapMethods("/", new[] { "GET", "HEAD" }, (HttpContext context) =>
            HttpMethods.IsHead(context.Request.Method) ? Results.Ok() : Results.Text("Alive"));
        app.MapMethods("/deeplink/reset-password", new[] { "GET", "HEAD" }, DeepLink);
        app.MapPost("/auth", (Func<HttpContext, Task<IResult>>)Authenticate).RequireRateLimiting("auth");
        app.MapPost("/forgot-password", (Func<HttpContext, Task<IResult>>)ForgotPassword).RequireRateLimiting("forgot-password");
        app.MapPost("/reset-password", (Func<HttpContext, Task<IResult>>)ResetPassword).RequireRateLimiting("reset-password");
        app.MapMethods("/me", new[] { "GET", "HEAD" }, Profile);
    }

    // Every protected route calls this; a signed token is not enough if the account
    // has been removed or its session version changed after a password reset.
    public static long? UserId(HttpContext context)
    {
        var authorization = context.Request.Headers.Authorization.ToString();
        if (!authorization.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase)) return null;
        var token = authorization[7..].Trim();
        if (token.Length == 0 || token.Length > 8192) return null;
        var parts = token.Split('.');
        if (parts.Length != 3 || !TryDecode(parts[0], out var headerBytes)
            || !TryDecode(parts[1], out var payloadBytes) || !TryDecode(parts[2], out var signature)
            || signature.Length != 32) return null;

        var signed = Encoding.ASCII.GetBytes(parts[0] + "." + parts[1]);
        var expected = HMACSHA256.HashData(jwtSecret, signed);
        if (!CryptographicOperations.FixedTimeEquals(signature, expected)) return null;

        long id;
        long version;
        try
        {
            using var header = JsonDocument.Parse(headerBytes);
            if (header.RootElement.ValueKind != JsonValueKind.Object
                || !header.RootElement.TryGetProperty("alg", out var alg)
                || alg.ValueKind != JsonValueKind.String || alg.GetString() != "HS256"
                || header.RootElement.TryGetProperty("crit", out _)) return null;
            using var payload = JsonDocument.Parse(payloadBytes);
            var root = payload.RootElement;
            if (root.ValueKind != JsonValueKind.Object
                || !root.TryGetProperty("iss", out var iss) || iss.ValueKind != JsonValueKind.String || iss.GetString() != issuer
                || !root.TryGetProperty("aud", out var aud) || !HasAudience(aud)
                || !root.TryGetProperty("exp", out var exp) || !exp.TryGetInt64(out var expiry)
                || expiry <= DateTimeOffset.UtcNow.ToUnixTimeSeconds()
                || !root.TryGetProperty("user_id", out var user) || !user.TryGetInt64(out id) || id <= 0
                || !root.TryGetProperty("v", out var session) || !session.TryGetInt64(out version) || version < 0)
                return null;
        }
        catch (JsonException)
        {
            return null;
        }
        catch (InvalidOperationException)
        {
            return null;
        }

        using var connection = Database.Open();
        using var command = connection.CreateCommand();
        command.CommandText = "SELECT session_version FROM users WHERE id = @id";
        Parameter(command, "@id", id);
        var result = command.ExecuteScalar();
        return result is not null && result is not DBNull
            && Convert.ToInt64(result, CultureInfo.InvariantCulture) == version ? id : null;
    }

    private static async Task<IResult> Authenticate(HttpContext context)
    {
        NoStore(context);
        var body = await ReadBody(context, RequestJson.AuthRequest);
        if (body?.Email is null || body.Password is null) return Text("Invalid request", 400);
        if (body.Password.Length is < 2 or > 24)
            return Text("400: java.lang.IllegalArgumentException: Password should be from 2 to 24 characters long", 400);
        if (body.Email.Length is < 5 or > 320 || !body.Email.Contains('@') || body.Email.Any(char.IsControl))
            return Text("400: java.lang.IllegalArgumentException: Invalid email address", 400);

        using var connection = Database.Open();
        var account = FindAccount(connection, body.Email);
        if (account is null && allowRegistration)
        {
            var hash = HashPassword(body.Password);
            try
            {
                using var command = connection.CreateCommand();
                command.CommandText = "INSERT INTO users (email, password_hash) VALUES (@email, @password)";
                Parameter(command, "@email", body.Email);
                Parameter(command, "@password", hash);
                command.ExecuteNonQuery();
                account = FindAccount(connection, body.Email);
                if (account is not null && account.SessionVersion == 0
                    && CryptographicOperations.FixedTimeEquals(
                        Encoding.UTF8.GetBytes(account.PasswordHash), Encoding.UTF8.GetBytes(hash)))
                    return Results.Json(new TokenResponse(CreateToken(account.Id, account.SessionVersion)), ResponseJson.TokenResponse);
            }
            catch (DbException exception) when (exception is Microsoft.Data.Sqlite.SqliteException { SqliteExtendedErrorCode: 2067 }
                or MySqlConnector.MySqlException { Number: 1062 })
            {
                // A concurrent first login may have inserted this email.
                account = FindAccount(connection, body.Email);
                if (account is null) throw;
            }
        }
        if (account is null || !VerifyPassword(body.Password, account.PasswordHash))
            return Text("Wrong password", 400);
        return Results.Json(new TokenResponse(CreateToken(account.Id, account.SessionVersion)), ResponseJson.TokenResponse);
    }

    private static async Task<IResult> ForgotPassword(HttpContext context)
    {
        NoStore(context);
        var body = await ReadBody(context, RequestJson.ForgotRequest);
        if (body?.Email is null) return Text("Invalid request", 400);
        if (body.Email.Length <= 320 && !body.Email.Any(char.IsControl))
        {
            var token = Convert.ToHexStringLower(RandomNumberGenerator.GetBytes(32));
            var tokenHash = TokenHash(token);
            var now = DateTimeOffset.UtcNow.ToUnixTimeSeconds();
            var updated = 0;
            using (var connection = Database.Open())
            using (var transaction = connection.BeginTransaction())
            using (var command = connection.CreateCommand())
            {
                command.Transaction = transaction;
                command.CommandText = """
                    UPDATE users SET password_reset_token_hash = @hash, password_reset_token_expires_at = @expires
                    WHERE email = @email AND (password_reset_token_hash IS NULL
                        OR password_reset_token_expires_at IS NULL OR password_reset_token_expires_at <= @now)
                    """;
                Parameter(command, "@hash", tokenHash);
                Parameter(command, "@expires", now + 900);
                Parameter(command, "@email", body.Email);
                Parameter(command, "@now", now);
                updated = command.ExecuteNonQuery();
                transaction.Commit();
            }
            if (updated == 1)
            {
                try
                {
                    await SendResetMail(body.Email, token);
                }
                catch (Exception exception) when (exception is not OutOfMemoryException)
                {
                    // Allow another request after an SMTP failure, without erasing a newer token.
                    using var connection = Database.Open();
                    using var command = connection.CreateCommand();
                    command.CommandText = "UPDATE users SET password_reset_token_hash = NULL, password_reset_token_expires_at = NULL WHERE password_reset_token_hash = @hash";
                    Parameter(command, "@hash", tokenHash);
                    command.ExecuteNonQuery();
                    Console.Error.WriteLine("Password reset email delivery failed.");
                }
            }
        }
        return Text("A password reset email was sent", 200);
    }

    private static async Task<IResult> ResetPassword(HttpContext context)
    {
        NoStore(context);
        var body = await ReadBody(context, RequestJson.ResetRequest);
        if (body?.ResetToken is null || body.Password is null) return Text("Invalid request", 400);
        if (body.ResetToken.Length > 1024) return Text("Invalid or expired token", 400);
        var hash = TokenHash(body.ResetToken);
        var now = DateTimeOffset.UtcNow.ToUnixTimeSeconds();
        using var connection = Database.Open();
        using (var check = connection.CreateCommand())
        {
            check.CommandText = "SELECT id FROM users WHERE password_reset_token_hash = @hash AND password_reset_token_expires_at > @now";
            Parameter(check, "@hash", hash);
            Parameter(check, "@now", now);
            if (check.ExecuteScalar() is null) return Text("Invalid or expired token", 400);
        }
        if (body.Password.Length is < 2 or > 24)
            return Text("Password should be from 2 to 24 characters long", 400);
        var passwordHash = HashPassword(body.Password);
        using var transaction = connection.BeginTransaction();
        using var command = connection.CreateCommand();
        command.Transaction = transaction;
        command.CommandText = """
            UPDATE users SET password_hash = @password, password_reset_token_hash = NULL,
                password_reset_token_expires_at = NULL, session_version = session_version + 1
            WHERE password_reset_token_hash = @hash AND password_reset_token_expires_at > @now
            """;
        Parameter(command, "@password", passwordHash);
        Parameter(command, "@hash", hash);
        Parameter(command, "@now", DateTimeOffset.UtcNow.ToUnixTimeSeconds());
        var updated = command.ExecuteNonQuery();
        transaction.Commit();
        return updated == 1 ? Text("Password has been reset successfully", 200) : Text("Invalid or expired token", 400);
    }

    private static IResult Profile(HttpContext context)
    {
        NoStore(context);
        var id = UserId(context);
        if (id is null) return Results.Unauthorized();
        using var connection = Database.Open();
        using var command = connection.CreateCommand();
        command.CommandText = "SELECT email, nickname FROM users WHERE id = @id";
        Parameter(command, "@id", id.Value);
        using var reader = command.ExecuteReader();
        if (!reader.Read()) return Results.Unauthorized();
        if (HttpMethods.IsHead(context.Request.Method)) return Results.Ok();
        return Results.Json(new ProfileResponse(id.Value, reader.GetString(0), reader.IsDBNull(1) ? null : reader.GetString(1)), ResponseJson.ProfileResponse);
    }

    private static IResult DeepLink(HttpContext context)
    {
        NoStore(context);
        context.Response.Headers["Referrer-Policy"] = "no-referrer";
        context.Response.Headers["X-Content-Type-Options"] = "nosniff";
        var token = context.Request.Query["token"].ToString();
        if (string.IsNullOrWhiteSpace(token)) return Text("Missing token", 400);
        if (HttpMethods.IsHead(context.Request.Method)) return Results.Ok();
        var link = $"kotatsu://reset-password?base_url={Uri.EscapeDataString(baseUrl)}&token={Uri.EscapeDataString(token)}";
        var scriptLink = JsonSerializer.Serialize(link, AuthJsonContext.Default.String);
        var htmlLink = HtmlEncoder.Default.Encode(link);
        var page = $$"""
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
                    window.location.href = {{scriptLink}};
                </script>
            </head>
            <body>
            <main>
                <p>If you are not redirected automatically, <a href="{{htmlLink}}">click here</a>.</p>
            </main>
            </body>
            </html>
            """;
        return Results.Text(page, "text/html", Encoding.UTF8);
    }

    private static async Task SendResetMail(string email, string token)
    {
        var link = $"{baseUrl}/deeplink/reset-password?token={Uri.EscapeDataString(token)}";
        var text = $"You can reset your password at {link}";
        if (mailProvider == "console")
        {
            Console.WriteLine($"=== ConsoleMail ===\nTo: {email}\nSubject: Password reset\nBody (plain):\n{text}\n==================");
            return;
        }
        if (!MailboxAddress.TryParse(email, out var recipient)
            || !string.Equals(recipient.Address, email, StringComparison.OrdinalIgnoreCase))
            throw new FormatException("The account email is not a single mailbox address");
        var safeLink = HtmlEncoder.Default.Encode(link);
        var html = $$"""
            <!DOCTYPE html>
            <html lang="en">
            <head>
                <meta charset="UTF-8">
                <title>Forgot password</title>
            </head>
            <body style="font-family:-apple-system,'Segoe UI',Roboto,Arial,sans-serif;">
            <main>
                <p>We received a request to reset your password.</p>
                <p>You can reset your password at <a href="{{safeLink}}">{{safeLink}}</a></p>
                <p>If you didn't make this request, please ignore this email.</p>
            </main>
            </body>
            </html>
            """;
        var message = new MimeMessage();
        message.From.Add(smtpFrom!);
        message.To.Add(recipient);
        message.Subject = "Password reset";
        message.Body = new BodyBuilder { TextBody = text, HtmlBody = html }.ToMessageBody();
        using var client = new MailKit.Net.Smtp.SmtpClient { Timeout = 15_000 };
        await client.ConnectAsync(smtpHost, smtpPort, smtpSecurity);
        if (smtpUsername.Length != 0) await client.AuthenticateAsync(smtpUsername, smtpPassword);
        await client.SendAsync(message);
        await client.DisconnectAsync(true);
    }

    private static string CreateToken(long id, long version)
    {
        var payload = JsonSerializer.SerializeToUtf8Bytes(
            new JwtPayload(issuer, [audience], id, version, DateTimeOffset.UtcNow.AddDays(30).ToUnixTimeSeconds()),
            AuthJsonContext.Default.JwtPayload);
        var signed = JwtHeader + "." + Encode(payload);
        var signature = HMACSHA256.HashData(jwtSecret, Encoding.ASCII.GetBytes(signed));
        return signed + "." + Encode(signature);
    }

    private static bool HasAudience(JsonElement element)
    {
        if (element.ValueKind == JsonValueKind.String) return element.GetString() == audience;
        if (element.ValueKind != JsonValueKind.Array) return false;
        foreach (var item in element.EnumerateArray())
            if (item.ValueKind == JsonValueKind.String && item.GetString() == audience) return true;
        return false;
    }

    private static string Encode(byte[] data) => Convert.ToBase64String(data).TrimEnd('=').Replace('+', '-').Replace('/', '_');

    private static bool TryDecode(string value, out byte[] decoded)
    {
        decoded = Array.Empty<byte>();
        if (value.Length == 0 || value.Length % 4 == 1) return false;
        foreach (var ch in value)
            if (!(ch is >= 'A' and <= 'Z' or >= 'a' and <= 'z' or >= '0' and <= '9' or '-' or '_')) return false;
        try
        {
            var padded = value.Replace('-', '+').Replace('_', '/').PadRight((value.Length + 3) / 4 * 4, '=');
            decoded = Convert.FromBase64String(padded);
            return true;
        }
        catch (FormatException)
        {
            return false;
        }
    }

    private static byte[] DerivePassword(string password, byte[] salt)
    {
        using var argon = new Argon2id(Encoding.UTF8.GetBytes(password))
        {
            Salt = salt,
            MemorySize = PasswordMemoryKiB,
            Iterations = PasswordIterations,
            DegreeOfParallelism = PasswordParallelism
        };
        return argon.GetBytes(32);
    }

    private static string HashPassword(string password)
    {
        var salt = RandomNumberGenerator.GetBytes(16);
        var hash = DerivePassword(password, salt);
        return PasswordPrefix + Convert.ToBase64String(salt).TrimEnd('=') + "$"
            + Convert.ToBase64String(hash).TrimEnd('=');
    }

    private static bool VerifyPassword(string password, string stored)
    {
        if (!stored.StartsWith(PasswordPrefix, StringComparison.Ordinal)) return false;
        var parts = stored[PasswordPrefix.Length..].Split('$');
        if (parts.Length != 2 || !TryDecodePasswordField(parts[0], 16, out var salt)
            || !TryDecodePasswordField(parts[1], 32, out var expected)) return false;
        var actual = DerivePassword(password, salt);
        return CryptographicOperations.FixedTimeEquals(actual, expected);
    }

    private static bool TryDecodePasswordField(string value, int length, out byte[] decoded)
    {
        decoded = Array.Empty<byte>();
        if (value.Length != (length * 8 + 5) / 6) return false;
        const string alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
        foreach (var ch in value)
            if (alphabet.IndexOf(ch) < 0) return false;
        // Unused bits in the final character must be zero for canonical unpadded base64.
        if ((alphabet.IndexOf(value[^1]) & (length == 16 ? 15 : 3)) != 0) return false;
        decoded = Convert.FromBase64String(value.PadRight((value.Length + 3) / 4 * 4, '='));
        return decoded.Length == length;
    }

    private static string TokenHash(string token) => Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(token)));

    private static Account? FindAccount(DbConnection connection, string email)
    {
        using var command = connection.CreateCommand();
        command.CommandText = "SELECT id, password_hash, session_version FROM users WHERE email = @email";
        Parameter(command, "@email", email);
        using var reader = command.ExecuteReader();
        return reader.Read()
            ? new Account(Convert.ToInt64(reader.GetValue(0), CultureInfo.InvariantCulture), reader.GetString(1),
                Convert.ToInt64(reader.GetValue(2), CultureInfo.InvariantCulture))
            : null;
    }

    private static void AddColumn(DbConnection connection, HashSet<string> columns, string name, string type)
    {
        if (columns.Contains(name)) return;
        using var command = connection.CreateCommand();
        command.CommandText = $"ALTER TABLE users ADD COLUMN {name} {type}";
        command.ExecuteNonQuery();
    }

    private static void Parameter(DbCommand command, string name, object value)
    {
        var parameter = command.CreateParameter();
        parameter.ParameterName = name;
        parameter.Value = value;
        command.Parameters.Add(parameter);
    }

    private static async Task<T?> ReadBody<T>(HttpContext context, JsonTypeInfo<T> typeInfo) where T : class
    {
        var size = context.Features.Get<IHttpMaxRequestBodySizeFeature>();
        if (size is { IsReadOnly: false }) size.MaxRequestBodySize = 8192;
        try
        {
            return await context.Request.ReadFromJsonAsync(typeInfo, context.RequestAborted);
        }
        catch (JsonException) { return null; }
        catch (BadHttpRequestException) { return null; }
        catch (InvalidOperationException) { return null; }
    }

    private static void NoStore(HttpContext context) => context.Response.Headers.CacheControl = "no-store";
    private static IResult Text(string body, int status) => Results.Text(body, "text/plain", Encoding.UTF8, status);

    private sealed record Account(long Id, string PasswordHash, long SessionVersion);
    internal sealed record AuthRequest([property: JsonPropertyName("email")] string? Email,
        [property: JsonPropertyName("password")] string? Password);
    internal sealed record ForgotRequest([property: JsonPropertyName("email")] string? Email);
    internal sealed record ResetRequest([property: JsonPropertyName("reset_token")] string? ResetToken,
        [property: JsonPropertyName("password")] string? Password);
    internal sealed record TokenResponse([property: JsonPropertyName("token")] string Token);
    internal sealed record ProfileResponse([property: JsonPropertyName("id")] long Id,
        [property: JsonPropertyName("email")] string Email, [property: JsonPropertyName("nickname")] string? Nickname);
    internal sealed record JwtPayload([property: JsonPropertyName("iss")] string Issuer,
        [property: JsonPropertyName("aud")] string[] Audience, [property: JsonPropertyName("user_id")] long UserId,
        [property: JsonPropertyName("v")] long Version, [property: JsonPropertyName("exp")] long Expires);
}

[JsonSourceGenerationOptions(GenerationMode = JsonSourceGenerationMode.Metadata)]
[JsonSerializable(typeof(Auth.AuthRequest))]
[JsonSerializable(typeof(Auth.ForgotRequest))]
[JsonSerializable(typeof(Auth.ResetRequest))]
[JsonSerializable(typeof(Auth.TokenResponse))]
[JsonSerializable(typeof(Auth.ProfileResponse))]
[JsonSerializable(typeof(Auth.JwtPayload))]
[JsonSerializable(typeof(string))]
internal partial class AuthJsonContext : JsonSerializerContext;
