using System.Data.Common;
using System.Globalization;
using Microsoft.Data.Sqlite;
using MySqlConnector;

public static class Database
{
    private static readonly object WalLock = new();
    private static bool walInitialized;
    public static string Dialect { get; } = ReadDialect();
    private static readonly string ConnectionString = MakeConnectionString();

    static Database()
    {
        // Root the bundled SQLite provider for NativeAOT instead of relying on reflection.
        if (Dialect == "sqlite") SQLitePCL.Batteries_V2.Init();
    }

    private static string ReadDialect()
    {
        var dialect = Environment.GetEnvironmentVariable("DATABASE_DIALECT")?.Trim().ToLowerInvariant();
        return dialect switch
        {
            null or "" or "sqlite" => "sqlite",
            "mysql" => "mysql",
            _ => throw new InvalidOperationException("DATABASE_DIALECT must be sqlite or mysql")
        };
    }

    private static string MakeConnectionString()
    {
        if (Dialect == "sqlite")
        {
            var path = Environment.GetEnvironmentVariable("DATABASE_PATH");
            if (string.IsNullOrWhiteSpace(path)) path = "kotatsu.db";
            return new SqliteConnectionStringBuilder
            {
                DataSource = path,
                Mode = SqliteOpenMode.ReadWriteCreate,
                DefaultTimeout = 30
            }.ToString();
        }

        var portText = Environment.GetEnvironmentVariable("DATABASE_PORT");
        if (string.IsNullOrWhiteSpace(portText)) portText = "3306";
        if (!uint.TryParse(portText, NumberStyles.None, CultureInfo.InvariantCulture, out var port)
            || port is 0 or > 65535)
            throw new InvalidOperationException("DATABASE_PORT must be a valid TCP port");

        return new MySqlConnectionStringBuilder
        {
            Server = Environment.GetEnvironmentVariable("DATABASE_HOST") ?? "127.0.0.1",
            Port = port,
            Database = Environment.GetEnvironmentVariable("DATABASE_NAME") ?? "kotatsu_db",
            UserID = Environment.GetEnvironmentVariable("DATABASE_USER") ?? "kotatsu",
            Password = Environment.GetEnvironmentVariable("DATABASE_PASSWORD") ?? "",
            AllowPublicKeyRetrieval = true,
            SslMode = MySqlSslMode.Preferred
        }.ConnectionString;
    }

    public static DbConnection Open()
    {
        DbConnection connection = Dialect == "sqlite"
            ? new SqliteConnection(ConnectionString)
            : new MySqlConnection(ConnectionString);
        try
        {
            connection.Open();
            if (Dialect == "sqlite")
            {
                lock (WalLock)
                {
                    if (!walInitialized)
                    {
                        using var journal = connection.CreateCommand();
                        journal.CommandText = "PRAGMA journal_mode=WAL";
                        var mode = Convert.ToString(journal.ExecuteScalar(), CultureInfo.InvariantCulture);
                        if (!string.Equals(mode, "wal", StringComparison.OrdinalIgnoreCase))
                            throw new InvalidOperationException("SQLite database must support WAL journaling");
                        walInitialized = true;
                    }
                }
                using var settings = connection.CreateCommand();
                settings.CommandText = "PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=30000";
                settings.ExecuteNonQuery();
            }
            return connection;
        }
        catch
        {
            connection.Dispose();
            throw;
        }
    }
}
