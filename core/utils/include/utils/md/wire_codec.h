#pragma once

#include <cstddef>
#include <cstdint>
#include <span>

#include "utils/md/wire.h"

namespace utils::md::wire {

enum class CodecError : std::uint8_t {
  Ok,
  BufferTooSmall,
  BadMagic,
  UnsupportedSchema,
  UnknownMessageType,
  LengthMismatch,
  InvalidField,
};

struct HeaderFields {
  InstrumentId instrument_id{};
  std::uint64_t bus_seq{};
  std::uint64_t source_seq{};
  std::uint64_t exchange_ts_ns{};
  std::uint64_t receive_tsc{};
  std::uint64_t publish_tsc{};
  std::uint32_t book_generation{};
  BookState state{};
  std::uint8_t source_id{};
  std::uint16_t flags{};
};

struct EncodeResult {
  CodecError error{CodecError::Ok};
  std::size_t size{};
  [[nodiscard]] explicit operator bool() const noexcept {
    return error == CodecError::Ok;
  }
};

[[nodiscard]] CodecError
ValidateHeader(std::span<const std::byte> bytes,
               RecordHeader *header = nullptr) noexcept;

[[nodiscard]] CodecError
DecodeInstrument(std::span<const std::byte> bytes,
                 InstrumentUpdateRecord &record) noexcept;
[[nodiscard]] CodecError
DecodeInstrumentCatalog(std::span<const std::byte> bytes,
                        InstrumentCatalogRecord &record) noexcept;
[[nodiscard]] CodecError DecodeBbo(std::span<const std::byte> bytes,
                                   BboRecord &record) noexcept;
[[nodiscard]] CodecError DecodeTicker(std::span<const std::byte> bytes,
                                      TickerRecord &record) noexcept;
[[nodiscard]] CodecError DecodeDelta(std::span<const std::byte> bytes,
                                     DeltaRecord &record) noexcept;
[[nodiscard]] CodecError
DecodeSnapshotBegin(std::span<const std::byte> bytes,
                    SnapshotBeginRecord &record) noexcept;
[[nodiscard]] CodecError
DecodeSnapshotChunk(std::span<const std::byte> bytes,
                    SnapshotChunkRecord &record) noexcept;
[[nodiscard]] CodecError
DecodeSnapshotEnd(std::span<const std::byte> bytes,
                  SnapshotEndRecord &record) noexcept;
[[nodiscard]] CodecError
DecodeAggBbo(std::span<const std::byte> bytes,
             AggBboRecord &record) noexcept;
[[nodiscard]] CodecError
DecodeAggOrderBook(std::span<const std::byte> bytes,
                   AggOrderBookRecord &record) noexcept;

[[nodiscard]] EncodeResult
EncodeInstrument(std::span<std::byte> destination,
                 const HeaderFields &header,
                 const Instrument &instrument) noexcept;
[[nodiscard]] EncodeResult
EncodeInstrumentCatalog(std::span<std::byte> destination,
                        const HeaderFields &header,
                        const InstrumentCatalog &catalog) noexcept;
[[nodiscard]] EncodeResult EncodeBbo(std::span<std::byte> destination,
                                     const HeaderFields &header,
                                     const Level &bid,
                                     const Level &ask) noexcept;
[[nodiscard]] EncodeResult EncodeTicker(std::span<std::byte> destination,
                                        const HeaderFields &header,
                                        const TickerEvent &event) noexcept;
[[nodiscard]] EncodeResult EncodeDelta(std::span<std::byte> destination,
                                       const HeaderFields &header, Side side,
                                       const Level &level) noexcept;
[[nodiscard]] EncodeResult
EncodeSnapshotBegin(std::span<std::byte> destination,
                    const HeaderFields &header, std::uint32_t level_count,
                    std::uint32_t chunk_count) noexcept;
[[nodiscard]] EncodeResult
EncodeSnapshotChunk(std::span<std::byte> destination,
                    const HeaderFields &header, std::uint32_t chunk_index,
                    Side side, std::span<const Level> levels) noexcept;
[[nodiscard]] EncodeResult
EncodeSnapshotEnd(std::span<std::byte> destination,
                  const HeaderFields &header, std::uint32_t received_levels,
                  std::uint32_t checksum) noexcept;
[[nodiscard]] EncodeResult
EncodeAggBbo(std::span<std::byte> destination,
             const HeaderFields &header,
             const AggBboRecord &value) noexcept;
[[nodiscard]] EncodeResult
EncodeAggOrderBook(std::span<std::byte> destination,
                   const HeaderFields &header,
                   const AggOrderBookRecord &value) noexcept;

class RecordVisitor {
 public:
  virtual ~RecordVisitor() = default;
  virtual bool OnInstrument(const InstrumentUpdateRecord &) noexcept = 0;
  virtual bool OnInstrumentCatalog(
      const InstrumentCatalogRecord &) noexcept {
    return true;
  }
  virtual bool OnBbo(const BboRecord &) noexcept = 0;
  virtual bool OnTicker(const TickerRecord &) noexcept = 0;
  virtual bool OnDelta(const DeltaRecord &) noexcept = 0;
  virtual bool OnSnapshotBegin(const SnapshotBeginRecord &) noexcept = 0;
  virtual bool OnSnapshotChunk(const SnapshotChunkRecord &) noexcept = 0;
  virtual bool OnSnapshotEnd(const SnapshotEndRecord &) noexcept = 0;
};

// Copies into an aligned stack object before dispatch, so consumers may decode
// records from unaligned ring payloads without aliasing or lifetime hazards.
[[nodiscard]] CodecError Decode(std::span<const std::byte> bytes,
                                RecordVisitor &visitor) noexcept;

}  // namespace utils::md::wire
