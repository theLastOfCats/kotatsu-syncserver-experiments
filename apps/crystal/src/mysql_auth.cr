require "mysql"
require "digest/sha1"
require "digest/sha256"

# crystal-mysql 0.18 implements the database protocol but not MySQL 8's
# default authentication. Extend its connection handshake, retaining the
# shard's query, transaction and connection-pool implementations.
class MySql::Connection
  private def scramble(password : String, salt : Bytes, plugin : String) : Bytes
    return Bytes.empty if password.empty?
    case plugin
    when "caching_sha2_password"
      first = Digest::SHA256.digest(password)
      second = Digest::SHA256.digest(first)
      final = Digest::SHA256.digest { |digest| digest.update(second); digest.update(salt) }
    when "mysql_native_password"
      first = Digest::SHA1.digest(password)
      second = Digest::SHA1.digest(first)
      final = Digest::SHA1.digest { |digest| digest.update(salt); digest.update(second) }
    else
      raise "Unsupported MySQL authentication plugin #{plugin}"
    end
    first.size.times { |i| first[i] ^= final[i] }
    first
  end

  def initialize(options : ::DB::Connection::Options, mysql_options : ::MySql::Connection::Options)
    super(options)
    @socket = uninitialized UNIXSocket | TCPSocket | OpenSSL::SSL::Socket::Client
    begin
      transport = mysql_options.transport
      hostname = transport.host
      @socket = case transport.scheme
                when "tcp"
                  TCPSocket.new(hostname || raise("Missing MySQL host"), transport.port)
                when "unix"
                  UNIXSocket.new(transport.path)
                else
                  raise "Unsupported MySQL transport"
                end

      capabilities = 0_u32
      salt = Bytes.new(20)
      plugin = "mysql_native_password"
      read_packet do |packet|
        raise "Unsupported MySQL protocol" unless packet.read_byte! == 10
        @server_version = packet.read_string
        packet.read_fixed_int(4)
        packet.read_fully(salt[0, 8])
        packet.read_byte!
        capabilities = packet.read_fixed_int(2).to_u32
        packet.read_byte!
        packet.read_fixed_int(2)
        capabilities |= packet.read_fixed_int(2).to_u32 << 16
        auth_length = packet.read_byte!.to_i
        packet.read_byte_array(10)
        tail_length = {13, auth_length - 8}.max
        tail = packet.read_byte_array(tail_length)
        12.times { |i| salt[i + 8] = tail[i] }
        plugin = packet.read_string if capabilities & 0x00080000_u32 != 0
        plugin = "mysql_native_password" if plugin.empty?
      end

      flags = 0x00000001_u32 | 0x00000004_u32 | 0x00000200_u32 |
              0x00002000_u32 | 0x00008000_u32 | 0x00080000_u32
      flags |= 0x00000008_u32 if mysql_options.initial_catalog
      flags &= capabilities
      encrypted = false
      sequence = 0
      ssl_mode = mysql_options.ssl_options.mode
      ssl_available = capabilities & 0x00000800_u32 != 0
      if ssl_mode != SSLMode::Disabled && ssl_available && transport.scheme == "tcp"
        flags |= 0x00000800_u32
        sequence += 1
        write_packet(sequence) do |packet|
          packet.write_bytes(flags, IO::ByteFormat::LittleEndian)
          packet.write_bytes(0_u32, IO::ByteFormat::LittleEndian)
          packet.write_byte(MySql::Collations.id_for_collation(mysql_options.charset).to_u8)
          23.times { packet.write_byte(0_u8) }
        end
        @socket = OpenSSL::SSL::Socket::Client.new(@socket, context: mysql_options.ssl_options.build_context,
          sync_close: true, hostname: hostname)
        encrypted = true
      elsif ssl_mode != SSLMode::Disabled && ssl_mode != SSLMode::Preferred && transport.scheme != "unix"
        raise "MySQL server does not support TLS"
      end

      sequence += 1
      password = mysql_options.password || ""
      response = scramble(password, salt, plugin)
      write_packet(sequence) do |packet|
        packet.write_bytes(flags, IO::ByteFormat::LittleEndian)
        packet.write_bytes(0_u32, IO::ByteFormat::LittleEndian)
        packet.write_byte(MySql::Collations.id_for_collation(mysql_options.charset).to_u8)
        23.times { packet.write_byte(0_u8) }
        packet << (mysql_options.username || "") << '\0'
        packet.write_byte(response.size.to_u8)
        packet.write(response)
        if catalog = mysql_options.initial_catalog
          packet << catalog << '\0'
        end
        packet << plugin << '\0'
      end

      loop do
        sequence += 1
        status = 0_u8
        stage = 0_u8
        switched_salt = Bytes.empty
        read_packet do |packet|
          status = packet.read_byte!
          case status
          when 0xff
            handle_err_packet(packet)
          when 0xfe
            plugin = packet.read_string
            raw = [] of UInt8
            while byte = packet.read_byte
              raw << byte
            end
            length = {raw.size, 20}.min
            switched_salt = Bytes.new(length) { |i| raw[i] }
          when 0x01
            stage = packet.read_byte!
          when 0x00
          else
            raise "Unexpected MySQL authentication packet #{status}"
          end
        end
        break if status == 0
        if status == 0xfe
          salt = switched_salt
          sequence += 1
          write_packet(sequence) { |packet| packet.write(scramble(password, salt, plugin)) }
        elsif stage == 0x03
          # Fast authentication succeeded; an ordinary OK packet follows.
          next
        elsif stage == 0x04
          raise "MySQL full authentication requires TLS" unless encrypted || transport.scheme == "unix"
          sequence += 1
          write_packet(sequence) { |packet| packet << password << '\0' }
        else
          raise "Unsupported MySQL authentication stage #{stage}"
        end
      end
    rescue IO::Error
      raise DB::ConnectionRefused.new
    end
  end
end
