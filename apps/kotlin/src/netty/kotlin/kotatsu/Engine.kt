package kotatsu

import io.ktor.server.engine.embeddedServer
import io.ktor.server.netty.Netty

internal fun startServer(config: Config, database: Database) {
    embeddedServer(Netty, host = config.bindHost, port = config.port) {
        applicationModule(database, config)
    }.start(wait = true)
}
