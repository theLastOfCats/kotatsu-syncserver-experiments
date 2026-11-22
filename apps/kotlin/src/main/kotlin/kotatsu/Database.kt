package kotatsu

import com.zaxxer.hikari.HikariConfig
import com.zaxxer.hikari.HikariDataSource
import org.sqlite.SQLiteConfig
import org.sqlite.SQLiteDataSource
import java.net.URI
import java.net.URLEncoder
import java.nio.charset.StandardCharsets
import java.sql.Connection

class Database(config: Config) : AutoCloseable {
    val dialect = config.dialect
    private val pool: HikariDataSource

    init {
        val settings = HikariConfig().apply {
            poolName = "kotatsu-$dialect"
            connectionTimeout = 10_000
            if (dialect == "sqlite") {
                val sqlite = SQLiteConfig().apply {
                    setBusyTimeout(10_000)
                    setJournalMode(SQLiteConfig.JournalMode.WAL)
                    setSynchronous(SQLiteConfig.SynchronousMode.FULL)
                    enforceForeignKeys(true)
                }
                dataSource = SQLiteDataSource(sqlite).apply { url = "jdbc:sqlite:${config.databasePath}" }
                // One connection serializes SQLite transactions, including read/merge/write cycles.
                maximumPoolSize = 1
                minimumIdle = 1
            } else {
                val host = config.databaseHost.removeSurrounding("[", "]")
                val authority = URI("mysql", null, host, config.databasePort, null, null, null).rawAuthority
                    ?: throw IllegalArgumentException("Invalid DATABASE_HOST")
                val name = URLEncoder.encode(config.databaseName, StandardCharsets.UTF_8).replace("+", "%20")
                jdbcUrl = "jdbc:mysql://$authority/$name?sslMode=PREFERRED&allowPublicKeyRetrieval=true"
                username = config.databaseUser
                password = config.databasePassword
                maximumPoolSize = 16
                minimumIdle = 1
            }
        }
        pool = HikariDataSource(settings)
    }

    fun <T> transaction(userId: Long? = null, block: (Connection) -> T): T {
        pool.connection.use { connection ->
            connection.autoCommit = false
            try {
                if (dialect == "mysql" && userId != null) {
                    connection.prepareStatement("SELECT id FROM users WHERE id = ? FOR UPDATE").use { statement ->
                        statement.setLong(1, userId)
                        statement.executeQuery().use { it.next() }
                    }
                }
                val result = block(connection)
                connection.commit()
                return result
            } catch (failure: Throwable) {
                try {
                    connection.rollback()
                } catch (rollbackFailure: Throwable) {
                    failure.addSuppressed(rollbackFailure)
                }
                throw failure
            } finally {
                connection.autoCommit = true
            }
        }
    }

    override fun close() {
        pool.close()
    }
}
