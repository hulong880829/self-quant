#include "oms/exchange/polymarket/trade_adapter.h"

#include <algorithm>
#include <array>
#include <charconv>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <limits>
#include <string_view>

#include <openssl/crypto.h>

namespace oms::exchange::polymarket {
namespace {

template <typename Id>
std::string_view IdView(const Id& id) noexcept {
  return id.length <= id.value.size()
             ? std::string_view{id.value.data(), id.length}
             : std::string_view{};
}

AdapterEvent Event(AdapterIdentity identity, AdapterEventKind kind) noexcept {
  AdapterEvent event{};
  event.kind = kind;
  event.source = identity;
  return event;
}

AdapterResult ProtocolAdapterResult(ProtocolResult result) noexcept {
  switch (result) {
    case ProtocolResult::Ok:
      return AdapterResult::Ok;
    case ProtocolResult::InvalidArgument:
    case ProtocolResult::Malformed:
      return AdapterResult::InvalidArgument;
    case ProtocolResult::Unsupported:
      return AdapterResult::Unsupported;
    case ProtocolResult::BoundsExceeded:
      return AdapterResult::WouldBlock;
  }
  return AdapterResult::Failed;
}

AdapterResult CryptoAdapterResult(CryptoResult result) noexcept {
  switch (result) {
    case CryptoResult::Ok:
      return AdapterResult::Ok;
    case CryptoResult::InvalidArgument:
      return AdapterResult::InvalidArgument;
    case CryptoResult::BufferTooSmall:
      return AdapterResult::WouldBlock;
    case CryptoResult::UnsupportedSignatureType:
      return AdapterResult::Unsupported;
    case CryptoResult::CryptoFailure:
      return AdapterResult::Failed;
  }
  return AdapterResult::Failed;
}

std::uint64_t Power10(std::uint8_t exponent) noexcept {
  std::uint64_t value = 1;
  while (exponent-- != 0) value *= 10U;
  return value;
}

bool AtScale(api::FixedPoint value, std::uint8_t scale,
             std::uint64_t& output) noexcept {
  output = 0;
  if (value.value <= 0 || value.scale > 18 || scale > 18)
    return false;
  std::uint64_t converted = static_cast<std::uint64_t>(value.value);
  if (value.scale < scale) {
    const std::uint64_t multiplier =
        Power10(static_cast<std::uint8_t>(scale - value.scale));
    if (converted > std::numeric_limits<std::uint64_t>::max() / multiplier)
      return false;
    converted *= multiplier;
  } else if (value.scale > scale) {
    const std::uint64_t divisor =
        Power10(static_cast<std::uint8_t>(value.scale - scale));
    if (converted % divisor != 0) return false;
    converted /= divisor;
  }
  output = converted;
  return true;
}

bool EncodeAddress(const Address& address, FixedText<42>& output) noexcept {
  constexpr char kDigits[] = "0123456789abcdef";
  std::array<char, 42> text{};
  text[0] = '0';
  text[1] = 'x';
  for (std::size_t index = 0; index < address.size(); ++index) {
    text[2 + index * 2] = kDigits[address[index] >> 4U];
    text[3 + index * 2] = kDigits[address[index] & 0x0fU];
  }
  return output.assign({text.data(), text.size()});
}

void RedactHeadTail(std::string_view text, std::size_t head, std::size_t tail,
                    char* output, std::size_t capacity,
                    std::size_t& size) noexcept {
  size = 0;
  if (output == nullptr || capacity == 0) return;
  if (text.size() < head + tail) {
    constexpr std::string_view kMasked = "****";
    const std::size_t copied = std::min(kMasked.size(), capacity);
    std::copy_n(kMasked.begin(), copied, output);
    size = copied;
    return;
  }
  if (head + 3 + tail > capacity) return;
  std::copy_n(text.begin(), head, output);
  output[head] = '.';
  output[head + 1] = '.';
  output[head + 2] = '.';
  std::copy(text.end() - static_cast<std::ptrdiff_t>(tail), text.end(),
            output + head + 3);
  size = head + 3 + tail;
}

std::uint64_t SaturatingAdd(std::uint64_t left, std::uint64_t right) noexcept {
  return left > std::numeric_limits<std::uint64_t>::max() - right
             ? std::numeric_limits<std::uint64_t>::max()
             : left + right;
}

}  // namespace

void SecureWipe(void* data, std::size_t size) noexcept {
  if (data != nullptr && size != 0) OPENSSL_cleanse(data, size);
}

PolymarketTradeAdapter::PolymarketTradeAdapter(
    const AdapterConfig& config) noexcept
    : config_(config),
      signing_context_(config.credentials.private_key.view()) {
  bool valid =
      config_.resolve_polymarket_token != nullptr &&
      config_.transport != nullptr &&
      config_.now_ms != nullptr && config_.request_timeout_ns != 0 &&
      config_.reconnect_initial_ns != 0 && config_.reconnect_max_ns != 0 &&
      config_.reconnect_initial_ns <= config_.reconnect_max_ns &&
      config_.heartbeat_interval_ns != 0 &&
      config_.liveness_timeout_ns > config_.heartbeat_interval_ns &&
      !config_.credentials.private_key.view().empty() &&
      signing_context_.status() == CryptoResult::Ok &&
      EncodeAddress(signing_context_.signer_address(), derived_signer_text_);
  if (valid) {
    if (config_.signature_type == api::PolymarketSignatureType::Eoa) {
      if (!config_.credentials.funder_address.view().empty()) {
        valid = false;
      } else {
        effective_funder_binary_ = signing_context_.signer_address();
        effective_funder_text_ = derived_signer_text_;
      }
    } else if (config_.signature_type ==
                   api::PolymarketSignatureType::PolyProxy ||
               config_.signature_type ==
                   api::PolymarketSignatureType::Poly1271) {
      if (config_.credentials.funder_address.view().empty() ||
          DecodeAddress(config_.credentials.funder_address.view(),
                        effective_funder_binary_) != CryptoResult::Ok ||
          !EncodeAddress(effective_funder_binary_, effective_funder_text_)) {
        valid = false;
      }
    } else {
      valid = false;
    }
  }
  status_ = valid ? AdapterStatus::Authenticating : AdapterStatus::Failed;
  resume_status_ = status_;
}

PolymarketTradeAdapter::~PolymarketTradeAdapter() noexcept {
  ClearDerivedCredentials();
  WipeUserSubscription();
}

void PolymarketTradeAdapter::ClearDerivedCredentials() noexcept {
  config_.credentials.api_key.secure_clear();
  config_.credentials.api_secret.secure_clear();
  config_.credentials.passphrase.secure_clear();
}

void PolymarketTradeAdapter::WipeUserSubscription() noexcept {
  if (config_.transport != nullptr) {
    (void)config_.transport->set_user_subscription({});
  }
}

void PolymarketTradeAdapter::ScheduleCredentialRetry(
    std::uint64_t now_ns) noexcept {
  if (credential_retry_delay_ns_ == 0)
    credential_retry_delay_ns_ = config_.reconnect_initial_ns;
  const std::uint64_t delay = credential_retry_delay_ns_;
  credential_retry_due_ns_ = SaturatingAdd(now_ns, delay);
  const std::uint64_t next_delay =
      delay >= config_.reconnect_max_ns - delay
          ? config_.reconnect_max_ns
          : std::min(delay * 2U, config_.reconnect_max_ns);
  credential_retry_delay_ns_ = next_delay;
  std::fprintf(stderr,
               "OMS polymarket credential retry scheduled delay_ns=%llu\n",
               static_cast<unsigned long long>(delay));
}

AdapterResult PolymarketTradeAdapter::SubmitCredentialRequest(
    bool create) noexcept {
  if (config_.transport == nullptr || credential_inflight_)
    return AdapterResult::InvalidArgument;
  ClearDerivedCredentials();
  WipeUserSubscription();
  ClobAuthRequest request{};
  request.id = next_request_id_++;
  if (request.id == 0) request.id = next_request_id_++;
  const ProtocolResult built =
      create ? BuildCreateApiKey(request.wire) : BuildDeriveApiKey(request.wire);
  if (built != ProtocolResult::Ok) return AdapterResult::Failed;
  std::array<char, 42> address{};
  std::size_t address_size = 0;
  if (EncodeEip55Address(signing_context_.signer_address(), address.data(),
                         address.size(), address_size) != CryptoResult::Ok ||
      !request.poly_address.assign({address.data(), address_size}) ||
      !request.poly_nonce.assign("0"))
    return AdapterResult::Failed;
  const std::uint64_t timestamp_s =
      config_.now_ms(config_.clock_context) / 1000U;
  std::array<char, 24> timestamp{};
  const auto converted = std::to_chars(
      timestamp.data(), timestamp.data() + timestamp.size(), timestamp_s);
  if (converted.ec != std::errc{} ||
      !request.poly_timestamp.assign(
          {timestamp.data(),
           static_cast<std::size_t>(converted.ptr - timestamp.data())}))
    return AdapterResult::Failed;
  std::size_t signature_size = 0;
  if (signing_context_.SignClobAuth(
          request.poly_timestamp.view(), 0, request.poly_signature.value.data(),
          request.poly_signature.value.size(), signature_size) !=
          CryptoResult::Ok ||
      signature_size != request.poly_signature.value.size())
    return AdapterResult::Failed;
  request.poly_signature.size = static_cast<std::uint16_t>(signature_size);
  std::array<char, 24> signer_redacted{};
  std::size_t signer_redacted_size = 0;
  RedactHeadTail(request.poly_address.view(), 6, 4, signer_redacted.data(),
                 signer_redacted.size(), signer_redacted_size);
  std::fprintf(stderr,
               "OMS polymarket credential stage=%s started signer=%.*s nonce=0\n",
               create ? "create" : "derive",
               static_cast<int>(signer_redacted_size), signer_redacted.data());
  const AdapterResult submitted = config_.transport->submit_clob_auth(request);
  OPENSSL_cleanse(request.poly_signature.value.data(),
                  request.poly_signature.value.size());
  request.poly_signature.size = 0;
  if (submitted == AdapterResult::WouldBlock) {
    ScheduleCredentialRetry(SaturatingAdd(
        config_.now_ms(config_.clock_context) * 1'000'000ULL, 0));
    return AdapterResult::Ok;
  }
  if (submitted != AdapterResult::Ok) return submitted;
  credential_inflight_ = true;
  credential_request_id_ = request.id;
  credential_retry_due_ns_ = 0;
  const std::uint64_t now_ns =
      config_.now_ms(config_.clock_context) * 1'000'000ULL;
  credential_deadline_ns_ = SaturatingAdd(now_ns, config_.request_timeout_ns);
  return AdapterResult::Ok;
}

AdapterResult PolymarketTradeAdapter::BeginCredentialDerive() noexcept {
  if (credential_phase_ != CredentialPhase::FinalDerivingAfterCreate)
    credential_phase_ = CredentialPhase::Deriving;
  return SubmitCredentialRequest(false);
}

AdapterResult PolymarketTradeAdapter::BeginCredentialCreate() noexcept {
  credential_phase_ = CredentialPhase::Creating;
  post_create_probe_reason_ = PostCreateProbeReason::None;
  return SubmitCredentialRequest(true);
}

AdapterResult PolymarketTradeAdapter::RetryCurrentCredential() noexcept {
  if (credential_phase_ == CredentialPhase::Creating)
    return BeginCredentialCreate();
  return BeginCredentialDerive();
}

AdapterResult PolymarketTradeAdapter::FailCredential(
    const char* reason, std::uint16_t http_status,
    std::uint64_t now_ns) noexcept {
  std::fprintf(stderr,
               "OMS polymarket credential failed http_status=%u error=%s\n",
               static_cast<unsigned>(http_status), reason);
  status_ = AdapterStatus::Failed;
  resume_status_ = status_;
  AdapterEvent status_event = Event(identity(), AdapterEventKind::Status);
  status_event.status.identity = identity();
  status_event.status.status = status_;
  status_event.status.reason = AdapterResult::Failed;
  status_event.status.event_time_ns = now_ns;
  return queue(status_event);
}

AdapterResult PolymarketTradeAdapter::ApplyCredentialSuccess(
    std::string_view payload, std::uint64_t now_ns) noexcept {
  ApiKeyCredentials parsed{};
  if (ParseApiKeyCredentials(payload, parsed) != ProtocolResult::Ok ||
      !config_.credentials.api_key.secure_assign(parsed.api_key) ||
      !config_.credentials.api_secret.secure_assign(parsed.secret) ||
      !config_.credentials.passphrase.secure_assign(parsed.passphrase)) {
    ClearDerivedCredentials();
    return FailCredential("credential_response_malformed", 200, now_ns);
  }
  std::array<char, kMaximumWireBytes> subscribe{};
  std::size_t subscribe_size = 0;
  const ProtocolResult built = BuildUserSubscribe(
      config_.credentials.api_key.view(),
      config_.credentials.api_secret.view(),
      config_.credentials.passphrase.view(), subscribe.data(), subscribe.size(),
      subscribe_size);
  AdapterResult subscribed = AdapterResult::Failed;
  if (built == ProtocolResult::Ok) {
    subscribed = config_.transport->set_user_subscription(
        {subscribe.data(), subscribe_size});
  }
  OPENSSL_cleanse(subscribe.data(), subscribe.size());
  if (built != ProtocolResult::Ok || subscribed != AdapterResult::Ok) {
    ClearDerivedCredentials();
    WipeUserSubscription();
    return FailCredential("subscription_failed", 200, now_ns);
  }
  std::array<char, 24> signer_redacted{};
  std::size_t signer_redacted_size = 0;
  std::array<char, 42> eip55{};
  std::size_t eip55_size = 0;
  if (EncodeEip55Address(signing_context_.signer_address(), eip55.data(),
                         eip55.size(), eip55_size) == CryptoResult::Ok) {
    RedactHeadTail({eip55.data(), eip55_size}, 6, 4, signer_redacted.data(),
                   signer_redacted.size(), signer_redacted_size);
  }
  std::array<char, 16> key_redacted{};
  std::size_t key_redacted_size = 0;
  RedactHeadTail(config_.credentials.api_key.view(), 4, 4, key_redacted.data(),
                 key_redacted.size(), key_redacted_size);
  std::fprintf(stderr,
               "OMS polymarket credential stage=ready signer=%.*s api_key=%.*s\n",
               static_cast<int>(signer_redacted_size), signer_redacted.data(),
               static_cast<int>(key_redacted_size), key_redacted.data());
  credential_retry_due_ns_ = 0;
  credential_retry_delay_ns_ = 0;
  credential_phase_ = CredentialPhase::None;
  post_create_probe_reason_ = PostCreateProbeReason::None;
  status_ = AdapterStatus::Connecting;
  resume_status_ = status_;
  const AdapterResult connected = config_.transport->request_reconnect();
  if (connected != AdapterResult::Ok) {
    status_ = AdapterStatus::Failed;
    resume_status_ = status_;
  }
  AdapterEvent status_event = Event(identity(), AdapterEventKind::Status);
  status_event.status.identity = identity();
  status_event.status.status = status_;
  status_event.status.reason = connected;
  status_event.status.event_time_ns = now_ns;
  return queue(status_event);
}

AdapterResult PolymarketTradeAdapter::StartInternalReconcile(
    std::uint64_t now_ns) noexcept {
  reconnect_due_ns_ = 0;
  reconnect_delay_ns_ = config_.reconnect_initial_ns;
  heartbeat_due_ns_ = SaturatingAdd(now_ns, config_.heartbeat_interval_ns);
  pagination_ = {};
  reconcile_uncertain_ = false;
  for (OrderBinding& binding : orders_)
    if (binding.used) binding.reconcile_seen = false;
  reconcile_generation_ = next_reconnect_generation_--;
  if (reconcile_generation_ == 0)
    reconcile_generation_ = next_reconnect_generation_--;
  status_ = AdapterStatus::Reconciling;
  resume_status_ = status_;
  const AdapterResult submitted = submit_reconcile_page();
  if (submitted != AdapterResult::Ok) {
    reconcile_generation_ = 0;
    status_ = AdapterStatus::Failed;
    resume_status_ = status_;
    return submitted;
  }
  return AdapterResult::Ok;
}

AdapterResult PolymarketTradeAdapter::start_initial_session() noexcept {
  if (status_ != AdapterStatus::Authenticating || initial_connect_started_ ||
      config_.transport == nullptr) {
    return AdapterResult::InvalidArgument;
  }
  initial_connect_started_ = true;
  const AdapterResult result = BeginCredentialDerive();
  if (result != AdapterResult::Ok) {
    status_ = AdapterStatus::Failed;
    resume_status_ = status_;
    return result;
  }
  return AdapterResult::Ok;
}

AdapterIdentity PolymarketTradeAdapter::identity() const noexcept {
  AdapterIdentity result{};
  result.kind = AdapterKind::Polymarket;
  result.venue = utils::md::Venue::Polymarket;
  result.product_type = utils::md::ProductType::BinaryOption;
  return result;
}

AdapterStatus PolymarketTradeAdapter::status() const noexcept { return status_; }

AdapterCapabilities PolymarketTradeAdapter::capabilities() const noexcept {
  AdapterCapabilities result{};
  result.bits =
      static_cast<std::uint64_t>(AdapterCapability::Limit) |
      static_cast<std::uint64_t>(AdapterCapability::TifGtc) |
      static_cast<std::uint64_t>(AdapterCapability::TifGtd) |
      static_cast<std::uint64_t>(AdapterCapability::TifIoc) |
      static_cast<std::uint64_t>(AdapterCapability::TifFok) |
      static_cast<std::uint64_t>(AdapterCapability::TifFak) |
      static_cast<std::uint64_t>(AdapterCapability::PostOnly) |
      static_cast<std::uint64_t>(AdapterCapability::ReconcileOpenOrders) |
      static_cast<std::uint64_t>(AdapterCapability::UserOrderStream) |
      static_cast<std::uint64_t>(AdapterCapability::UserFillStream);
  return result;
}

AdapterResult PolymarketTradeAdapter::reserve_command(
    AdapterCommandKind kind, AdapterReservation& reservation) noexcept {
  reservation = {};
  const bool ready =
      status_ == AdapterStatus::Ready ||
      (status_ == AdapterStatus::Backpressured &&
       resume_status_ == AdapterStatus::Ready);
  const bool reconnect_cancel =
      kind == AdapterCommandKind::Cancel &&
      (status_ == AdapterStatus::Reconnecting ||
       (status_ == AdapterStatus::Backpressured &&
        resume_status_ == AdapterStatus::Reconnecting));
  if (!ready && !reconnect_cancel)
    return AdapterResult::NotReady;
  for (std::size_t index = 0; index < commands_.size(); ++index) {
    CommandSlot& slot = commands_[index];
    if (slot.reserved || slot.inflight) continue;
    if (++slot.generation == 0) ++slot.generation;
    slot.reserved = true;
    slot.kind = kind;
    reservation.adapter = AdapterKind::Polymarket;
    reservation.slot = static_cast<std::uint32_t>(index);
    reservation.generation = slot.generation;
    return AdapterResult::Ok;
  }
  if (status_ != AdapterStatus::Backpressured) resume_status_ = status_;
  status_ = AdapterStatus::Backpressured;
  return AdapterResult::WouldBlock;
}

bool PolymarketTradeAdapter::valid(AdapterReservation reservation,
                                   AdapterCommandKind kind) const noexcept {
  if (reservation.adapter != AdapterKind::Polymarket ||
      reservation.slot >= commands_.size() || reservation.generation == 0)
    return false;
  const CommandSlot& slot = commands_[reservation.slot];
  return slot.reserved && !slot.inflight && slot.kind == kind &&
         slot.generation == reservation.generation;
}

void PolymarketTradeAdapter::release(std::size_t slot) noexcept {
  const std::uint64_t generation = commands_[slot].generation;
  commands_[slot] = {};
  commands_[slot].generation = generation;
  refresh_backpressure();
}

void PolymarketTradeAdapter::refresh_backpressure() noexcept {
  if (status_ != AdapterStatus::Backpressured ||
      pending_size_ == pending_events_.size())
    return;
  for (const CommandSlot& slot : commands_) {
    if (!slot.reserved && !slot.inflight) {
      status_ = resume_status_;
      return;
    }
  }
}

void PolymarketTradeAdapter::cancel_reservation(
    AdapterReservation reservation) noexcept {
  if (reservation.adapter != AdapterKind::Polymarket ||
      reservation.slot >= commands_.size())
    return;
  CommandSlot& slot = commands_[reservation.slot];
  if (slot.reserved && !slot.inflight &&
      slot.generation == reservation.generation)
    release(reservation.slot);
}

AdapterResult PolymarketTradeAdapter::authorize(
    TransportRequest& request) noexcept {
  const std::uint64_t timestamp = config_.now_ms(config_.clock_context) / 1000U;
  std::array<char, 24> text{};
  const auto converted =
      std::to_chars(text.data(), text.data() + text.size(), timestamp);
  if (converted.ec != std::errc{} ||
      !request.poly_timestamp.assign(
          {text.data(), static_cast<std::size_t>(converted.ptr - text.data())}) ||
      !request.poly_address.assign(derived_signer_text_.view()) ||
      !request.poly_api_key.assign(config_.credentials.api_key.view()) ||
      !request.poly_passphrase.assign(config_.credentials.passphrase.view()))
    return AdapterResult::InvalidArgument;
  std::size_t signature_size = 0;
  std::string_view signed_path{request.wire.path.data(),
                               request.wire.path_size};
  if (const std::size_t query = signed_path.find('?');
      query != std::string_view::npos)
    signed_path = signed_path.substr(0, query);
  const CryptoResult signed_result = L2Signature(
      config_.credentials.api_secret.view(), request.poly_timestamp.view(),
      {request.wire.method.data(), request.wire.method_size},
      signed_path,
      {request.wire.body.data(), request.wire.body_size},
      request.poly_signature.value.data(), request.poly_signature.value.size(),
      signature_size);
  if (signed_result != CryptoResult::Ok)
    return CryptoAdapterResult(signed_result);
  request.poly_signature.size = static_cast<std::uint16_t>(signature_size);
  return AdapterResult::Ok;
}

AdapterResult PolymarketTradeAdapter::commit_place(
    AdapterReservation reservation,
    const AdapterPlaceCommand& command) noexcept {
  if (!valid(reservation, AdapterCommandKind::Place))
    return AdapterResult::StaleReservation;
  const std::size_t index = reservation.slot;
  const auto finish = [&](AdapterResult result) noexcept {
    release(index);
    return result;
  };
  if (status_ != AdapterStatus::Ready &&
      !(status_ == AdapterStatus::Backpressured &&
        resume_status_ == AdapterStatus::Ready))
    return finish(AdapterResult::NotReady);
  const api::NewOrderRequest& request = command.request;
  std::size_t order_slots = 0;
  for (const OrderBinding& binding : orders_)
    if (binding.used) ++order_slots;
  for (const CommandSlot& slot : commands_)
    if (slot.inflight && slot.kind == AdapterCommandKind::Place) ++order_slots;
  if (order_slots >= orders_.size())
    return finish(AdapterResult::WouldBlock);
  if (request.type != api::OrderType::Limit ||
      (request.flags & ~(static_cast<std::uint16_t>(api::OrderFlag::PostOnly))) !=
          0)
    return finish(AdapterResult::Unsupported);
  const std::uint64_t now_ms = config_.now_ms(config_.clock_context);
  std::uint64_t expiration_seconds = 0;
  if (request.time_in_force == api::TimeInForce::GTD) {
    if (request.expire_time_ns == 0 ||
        request.expire_time_ns % 1000000000ULL != 0 ||
        request.expire_time_ns / 1000000ULL <= now_ms)
      return finish(AdapterResult::InvalidArgument);
    expiration_seconds = request.expire_time_ns / 1000000000ULL;
  } else if (request.expire_time_ns != 0) {
    return finish(AdapterResult::InvalidArgument);
  }
  if (command.routing.kind != api::ExecutionRouteKind::Polymarket ||
      command.routing.venue !=
          static_cast<std::uint8_t>(utils::md::Venue::Polymarket))
    return finish(AdapterResult::InvalidArgument);
  const std::uint8_t signature_type =
      static_cast<std::uint8_t>(config_.signature_type);
  const bool negative_risk = command.routing.negative_risk;
  const std::int64_t minimum_order_size =
      command.routing.minimum_order_size;
  const std::uint32_t taker_delay_ms = command.routing.taker_delay_ms;
  const std::uint8_t price_scale = command.routing.price_scale;
  const std::uint8_t quantity_scale = command.routing.quantity_scale;
  const std::int64_t tick_size = command.routing.tick_size;
  const std::int64_t lot_size = command.routing.lot_size;
  if (command.routing.instrument_expiry_ns != 0 &&
      now_ms >= command.routing.instrument_expiry_ns / 1'000'000ULL)
    return finish(AdapterResult::InvalidArgument);
  std::uint64_t quantity_units = 0;
  std::uint64_t price_units = 0;
  if (lot_size <= 0 || tick_size <= 0 ||
      !AtScale(request.quantity, quantity_scale, quantity_units) ||
      !AtScale(request.price, price_scale, price_units) ||
      quantity_units %
              static_cast<std::uint64_t>(lot_size) !=
          0 ||
      price_units % static_cast<std::uint64_t>(tick_size) != 0 ||
      quantity_units <
          static_cast<std::uint64_t>(minimum_order_size) ||
      price_units >= Power10(price_scale) ||
      ((request.flags & api::OrderFlag::PostOnly) != 0 &&
       request.time_in_force != api::TimeInForce::GTC &&
       request.time_in_force != api::TimeInForce::GTD))
    return finish(AdapterResult::InvalidArgument);
  if (request.time_in_force == api::TimeInForce::GTD) {
    if (now_ms > std::numeric_limits<std::uint64_t>::max() -
                     taker_delay_ms ||
        request.expire_time_ns / 1000000ULL <=
            now_ms + taker_delay_ms)
      return finish(AdapterResult::InvalidArgument);
  }

  Order order{};
  order.maker = effective_funder_binary_;
  order.signer =
      config_.signature_type == api::PolymarketSignatureType::PolyProxy
          ? signing_context_.signer_address()
          : effective_funder_binary_;
  order.token_id = command.routing.polymarket.token_id;
  order.side = request.side == api::Side::Buy ? 0 : 1;
  if (request.side != api::Side::Buy && request.side != api::Side::Sell)
    return finish(AdapterResult::InvalidArgument);
  order.signature_type = signature_type;
  order.timestamp_ms = now_ms;
  order.negative_risk = negative_risk;
  CryptoResult crypto = GenerateSafeSalt(order.salt);
  if (crypto != CryptoResult::Ok)
    return finish(CryptoAdapterResult(crypto));
  const ProtocolResult amounts =
      OrderAmounts(request.side, request.quantity, request.price,
                   order.maker_amount, order.taker_amount);
  if (amounts != ProtocolResult::Ok)
    return finish(ProtocolAdapterResult(amounts));
  Signature signature{};
  crypto = SignOrder(order, signing_context_, signature);
  if (crypto != CryptoResult::Ok)
    return finish(CryptoAdapterResult(crypto));

  TransportRequest transport{};
  transport.id = next_request_id_++;
  if (transport.id == 0) transport.id = next_request_id_++;
  const PlaceOrderInput input{order, signature,
                              config_.credentials.api_key.view(),
                              request.time_in_force,
                              (request.flags & api::OrderFlag::PostOnly) != 0,
                              expiration_seconds};
  const ProtocolResult built = BuildPlaceOrder(input, transport.wire);
  if (built != ProtocolResult::Ok)
    return finish(ProtocolAdapterResult(built));
  AdapterResult result = authorize(transport);
  if (result != AdapterResult::Ok) return finish(result);

  CommandSlot& slot = commands_[index];
  slot.request_id = transport.id;
  slot.command_id = command.command_id;
  slot.handle = command.handle;
  slot.token = request.token;
  slot.instrument_id = request.instrument_id;
  slot.reserved = false;
  slot.inflight = true;
  result = config_.transport->submit(transport);
  if (result != AdapterResult::Ok) return finish(result);
  return AdapterResult::Ok;
}

const api::VenueOrderId* PolymarketTradeAdapter::find_order(
    api::OrderHandle handle) const noexcept {
  for (const OrderBinding& binding : orders_)
    if (binding.used && binding.handle == handle) return &binding.venue_order_id;
  return nullptr;
}

void PolymarketTradeAdapter::bind_order(
    api::OrderHandle handle, const api::VenueOrderId& venue_order_id,
    api::InstrumentId instrument_id) noexcept {
  for (OrderBinding& binding : orders_) {
    if (binding.used && binding.handle == handle) {
      binding.venue_order_id = venue_order_id;
      binding.instrument_id = instrument_id;
      return;
    }
  }
  for (OrderBinding& binding : orders_) {
    if (!binding.used) {
      binding.used = true;
      binding.reconcile_seen = false;
      binding.handle = handle;
      binding.venue_order_id = venue_order_id;
      binding.instrument_id = instrument_id;
      return;
    }
  }
}

AdapterResult PolymarketTradeAdapter::commit_cancel(
    AdapterReservation reservation,
    const AdapterCancelCommand& command) noexcept {
  if (!valid(reservation, AdapterCommandKind::Cancel))
    return AdapterResult::StaleReservation;
  const std::size_t index = reservation.slot;
  const auto finish = [&](AdapterResult result) noexcept {
    release(index);
    return result;
  };
  if (status_ != AdapterStatus::Ready &&
      !(status_ == AdapterStatus::Backpressured &&
        resume_status_ == AdapterStatus::Ready) &&
      status_ != AdapterStatus::Reconnecting &&
      !(status_ == AdapterStatus::Backpressured &&
        resume_status_ == AdapterStatus::Reconnecting))
    return finish(AdapterResult::NotReady);
  const api::VenueOrderId* venue_order_id =
      find_order(command.request.handle);
  if (venue_order_id == nullptr) return finish(AdapterResult::InvalidArgument);
  TransportRequest transport{};
  transport.id = next_request_id_++;
  if (transport.id == 0) transport.id = next_request_id_++;
  const ProtocolResult built =
      BuildCancelOrder(IdView(*venue_order_id), transport.wire);
  if (built != ProtocolResult::Ok)
    return finish(ProtocolAdapterResult(built));
  AdapterResult result = authorize(transport);
  if (result != AdapterResult::Ok) return finish(result);
  CommandSlot& slot = commands_[index];
  slot.request_id = transport.id;
  slot.command_id = command.command_id;
  slot.handle = command.request.handle;
  slot.token = command.request.request_token;
  slot.venue_order_id = *venue_order_id;
  slot.reserved = false;
  slot.inflight = true;
  result = config_.transport->submit(transport);
  if (result != AdapterResult::Ok) return finish(result);
  return AdapterResult::Ok;
}

PolymarketTradeAdapter::CommandSlot* PolymarketTradeAdapter::find_request(
    std::uint32_t request_id) noexcept {
  for (CommandSlot& slot : commands_)
    if (slot.inflight && slot.request_id == request_id) return &slot;
  return nullptr;
}

AdapterResult PolymarketTradeAdapter::queue(
    const AdapterEvent& event) noexcept {
  if (pending_size_ == pending_events_.size()) {
    if (status_ != AdapterStatus::Backpressured) resume_status_ = status_;
    status_ = AdapterStatus::Backpressured;
    return AdapterResult::WouldBlock;
  }
  const std::size_t tail =
      (static_cast<std::size_t>(pending_begin_) + pending_size_) %
      pending_events_.size();
  pending_events_[tail] = event;
  ++pending_size_;
  return AdapterResult::Ok;
}

AdapterResult PolymarketTradeAdapter::offer(
    const AdapterEventSink& sink, const AdapterEvent& event) noexcept {
  if (pending_size_ != 0) return queue(event);
  if (sink.on_event == nullptr) return AdapterResult::InvalidArgument;
  const AdapterResult result = sink.on_event(sink.context, event);
  return result == AdapterResult::WouldBlock ? queue(event) : result;
}

AdapterResult PolymarketTradeAdapter::submit_reconcile_page() noexcept {
  TransportRequest request{};
  request.id = next_request_id_++;
  if (request.id == 0) request.id = next_request_id_++;
  const ProtocolResult built = BuildOpenOrdersPage(pagination_, request.wire);
  if (built != ProtocolResult::Ok) return ProtocolAdapterResult(built);
  AdapterResult result = authorize(request);
  if (result != AdapterResult::Ok) return result;
  result = config_.transport->submit(request);
  if (result == AdapterResult::Ok) {
    reconcile_request_id_ = request.id;
    reconcile_deadline_ns_ = 0;
  }
  return result;
}

AdapterResult PolymarketTradeAdapter::submit_query_page() noexcept {
  if (!query_.active) return AdapterResult::InvalidArgument;
  TransportRequest request{};
  request.id = next_request_id_++;
  if (request.id == 0) request.id = next_request_id_++;
  ProtocolResult built = ProtocolResult::InvalidArgument;
  Transport* transport = config_.transport;
  const auto& query_request = query_.request.request;
  std::array<char, 78> token_decimal{};
  std::size_t token_decimal_size{};
  std::array<char, 66> condition_hex{};
  std::string_view asset_id;
  std::string_view market;
  if (query_request.scope == api::QueryScope::SingleInstrument) {
    if (query_request.instrument.kind !=
        api::ExecutionRouteKind::Polymarket) {
      return AdapterResult::InvalidArgument;
    }
    if (TokenIdToDecimal(query_request.instrument.polymarket.token_id,
                         token_decimal.data(), token_decimal.size(),
                         token_decimal_size) != CryptoResult::Ok) {
      return AdapterResult::InvalidArgument;
    }
    asset_id =
        std::string_view(token_decimal.data(), token_decimal_size);
    constexpr char digits[] = "0123456789abcdef";
    condition_hex[0] = '0';
    condition_hex[1] = 'x';
    for (std::size_t index = 0;
         index < query_request.instrument.polymarket.condition_id.size();
         ++index) {
      const std::uint8_t byte =
          query_request.instrument.polymarket.condition_id[index];
      condition_hex[2 + index * 2] = digits[byte >> 4U];
      condition_hex[3 + index * 2] = digits[byte & 0x0fU];
    }
    market = std::string_view(condition_hex.data(), condition_hex.size());
  }
  if (query_.kind == api::QueryKind::OpenOrders) {
    built = BuildOpenOrdersPage(query_.pagination, request.wire, asset_id);
    if (built == ProtocolResult::Ok) {
      const AdapterResult authorized = authorize(request);
      if (authorized != AdapterResult::Ok) return authorized;
    }
  } else {
    built = BuildPositions(effective_funder_text_.view(),
                           request.wire, market);
    transport = config_.data_transport;
  }
  if (built != ProtocolResult::Ok) return ProtocolAdapterResult(built);
  if (transport == nullptr) return AdapterResult::Unsupported;
  const AdapterResult submitted = transport->submit(request);
  if (submitted == AdapterResult::Ok) {
    query_.request_id = request.id;
    query_.deadline_ns = 0;
  }
  return submitted;
}

AdapterResult PolymarketTradeAdapter::query_open_orders(
    const AdapterQueryRequest& request,
    const AdapterEventSink& sink) noexcept {
  (void)sink;
  if (request.request.account_id == 0 || request.token.sequence == 0)
    return AdapterResult::InvalidArgument;
  if (query_.active) return AdapterResult::WouldBlock;
  query_ = {};
  query_.active = true;
  query_.kind = api::QueryKind::OpenOrders;
  query_.request = request;
  const AdapterResult result = submit_query_page();
  if (result != AdapterResult::Ok) query_ = {};
  return result;
}

AdapterResult PolymarketTradeAdapter::query_positions(
    const AdapterQueryRequest& request,
    const AdapterEventSink& sink) noexcept {
  (void)sink;
  if (request.request.account_id == 0 || request.token.sequence == 0)
    return AdapterResult::InvalidArgument;
  if (config_.data_transport == nullptr) return AdapterResult::Unsupported;
  if (query_.active) return AdapterResult::WouldBlock;
  query_ = {};
  query_.active = true;
  query_.kind = api::QueryKind::Positions;
  query_.request = request;
  const AdapterResult result = submit_query_page();
  if (result != AdapterResult::Ok) query_ = {};
  return result;
}

AdapterServiceResult PolymarketTradeAdapter::service_io(
    std::uint64_t now_ns, std::uint32_t event_budget,
    const AdapterEventSink& sink) noexcept {
  AdapterServiceResult result{};
  if (status_ == AdapterStatus::Failed || status_ == AdapterStatus::Stopped ||
      config_.transport == nullptr) {
    result.result = AdapterResult::NotReady;
    return result;
  }
  const bool session_ready =
      status_ == AdapterStatus::Ready ||
      (status_ == AdapterStatus::Backpressured &&
       resume_status_ == AdapterStatus::Ready);
  if (session_ready && last_session_activity_ns_ != 0 &&
      now_ns - last_session_activity_ns_ >= config_.liveness_timeout_ns) {
    std::fprintf(stderr,
                 "oms polymarket liveness timeout last_activity_ns=%llu "
                 "now_ns=%llu\n",
                 static_cast<unsigned long long>(last_session_activity_ns_),
                 static_cast<unsigned long long>(now_ns));
    config_.transport->close();
    reconnect_reconcile_required_ = true;
    heartbeat_due_ns_ = 0;
    reconnect_delay_ns_ = config_.reconnect_initial_ns;
    reconnect_due_ns_ =
        now_ns > std::numeric_limits<std::uint64_t>::max() -
                     reconnect_delay_ns_
            ? std::numeric_limits<std::uint64_t>::max()
            : now_ns + reconnect_delay_ns_;
    status_ = AdapterStatus::Reconnecting;
    resume_status_ = status_;
    AdapterEvent status_event = Event(identity(), AdapterEventKind::Status);
    status_event.status.identity = identity();
    status_event.status.status = status_;
    status_event.status.reason = AdapterResult::NotReady;
    status_event.status.event_time_ns = now_ns;
    if (queue(status_event) != AdapterResult::Ok) {
      result.result = AdapterResult::WouldBlock;
      return result;
    }
  } else if (session_ready && heartbeat_due_ns_ != 0 &&
             heartbeat_due_ns_ <= now_ns) {
    const AdapterResult heartbeat = config_.transport->send_heartbeat();
    if (heartbeat != AdapterResult::Ok) {
      result.result = heartbeat;
      return result;
    }
    heartbeat_due_ns_ =
        now_ns > std::numeric_limits<std::uint64_t>::max() -
                     config_.heartbeat_interval_ns
            ? std::numeric_limits<std::uint64_t>::max()
            : now_ns + config_.heartbeat_interval_ns;
  }
  if (heartbeat_due_ns_ != 0)
    result.next_deadline_ns = heartbeat_due_ns_;
  if (session_ready && last_session_activity_ns_ != 0) {
    const std::uint64_t liveness_due =
        last_session_activity_ns_ >
                std::numeric_limits<std::uint64_t>::max() -
                    config_.liveness_timeout_ns
            ? std::numeric_limits<std::uint64_t>::max()
            : last_session_activity_ns_ + config_.liveness_timeout_ns;
    if (result.next_deadline_ns == 0 ||
        liveness_due < result.next_deadline_ns)
      result.next_deadline_ns = liveness_due;
  }
  for (CommandSlot& slot : commands_) {
    if (!slot.inflight) continue;
    if (slot.deadline_ns == 0) {
      slot.deadline_ns =
          now_ns > std::numeric_limits<std::uint64_t>::max() -
                       config_.request_timeout_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + config_.request_timeout_ns;
    }
    if (result.next_deadline_ns == 0 ||
        slot.deadline_ns < result.next_deadline_ns)
      result.next_deadline_ns = slot.deadline_ns;
  }
  if (reconcile_request_id_ != 0) {
    if (reconcile_deadline_ns_ == 0) {
      reconcile_deadline_ns_ =
          now_ns > std::numeric_limits<std::uint64_t>::max() -
                       config_.request_timeout_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + config_.request_timeout_ns;
    }
    if (result.next_deadline_ns == 0 ||
        reconcile_deadline_ns_ < result.next_deadline_ns)
      result.next_deadline_ns = reconcile_deadline_ns_;
  }
  if (query_.active && query_.request_id != 0) {
    if (query_.deadline_ns == 0) {
      query_.deadline_ns =
          now_ns > std::numeric_limits<std::uint64_t>::max() -
                       config_.request_timeout_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + config_.request_timeout_ns;
    }
    if (result.next_deadline_ns == 0 ||
        query_.deadline_ns < result.next_deadline_ns)
      result.next_deadline_ns = query_.deadline_ns;
  }
  if (status_ == AdapterStatus::Authenticating) {
    if (credential_inflight_ && credential_deadline_ns_ != 0 &&
        now_ns >= credential_deadline_ns_) {
      credential_inflight_ = false;
      credential_request_id_ = 0;
      credential_deadline_ns_ = 0;
      if (credential_phase_ == CredentialPhase::Creating) {
        credential_phase_ = CredentialPhase::FinalDerivingAfterCreate;
        post_create_probe_reason_ = PostCreateProbeReason::CreateAmbiguous;
        const AdapterResult started = BeginCredentialDerive();
        if (started != AdapterResult::Ok) {
          status_ = AdapterStatus::Failed;
          resume_status_ = status_;
          result.result = started;
          return result;
        }
      } else {
        ScheduleCredentialRetry(now_ns);
      }
    }
    if (!credential_inflight_ && credential_retry_due_ns_ != 0 &&
        now_ns >= credential_retry_due_ns_) {
      credential_retry_due_ns_ = 0;
      const AdapterResult started = RetryCurrentCredential();
      if (started != AdapterResult::Ok) {
        status_ = AdapterStatus::Failed;
        resume_status_ = status_;
        result.result = started;
        return result;
      }
    }
    if (credential_retry_due_ns_ != 0 &&
        (result.next_deadline_ns == 0 ||
         credential_retry_due_ns_ < result.next_deadline_ns))
      result.next_deadline_ns = credential_retry_due_ns_;
    if (credential_inflight_ && credential_deadline_ns_ != 0 &&
        (result.next_deadline_ns == 0 ||
         credential_deadline_ns_ < result.next_deadline_ns))
      result.next_deadline_ns = credential_deadline_ns_;
  }
  while (result.events_processed < event_budget) {
    if (pending_size_ != 0) {
      if (sink.on_event == nullptr) {
        result.result = AdapterResult::InvalidArgument;
        return result;
      }
      const AdapterResult delivered =
          sink.on_event(sink.context, pending_events_[pending_begin_]);
      if (delivered != AdapterResult::Ok) {
        result.result = delivered;
        return result;
      }
      pending_begin_ = static_cast<std::uint16_t>(
          (pending_begin_ + 1U) % pending_events_.size());
      --pending_size_;
      ++result.events_processed;
      refresh_backpressure();
      continue;
    }

    TransportEvent transport_event{};
    AdapterResult polled = config_.transport->poll(transport_event);
    if (polled == AdapterResult::WouldBlock &&
        config_.data_transport != nullptr)
      polled = config_.data_transport->poll(transport_event);
    if (polled == AdapterResult::WouldBlock) return result;
    if (polled != AdapterResult::Ok) {
      result.result = polled;
      return result;
    }
    last_session_activity_ns_ = now_ns;

    if (credential_inflight_ &&
        transport_event.kind == TransportEventKind::HttpResponse &&
        transport_event.request_id == credential_request_id_) {
      credential_inflight_ = false;
      credential_request_id_ = 0;
      credential_deadline_ns_ = 0;
      const std::uint16_t http_status = transport_event.status_code;
      const bool creating = credential_phase_ == CredentialPhase::Creating;
      if (CredentialHttpSuccess(http_status)) {
        if (ApplyCredentialSuccess(transport_event.payload, now_ns) !=
            AdapterResult::Ok) {
          result.result = AdapterResult::WouldBlock;
          return result;
        }
        continue;
      }
      if (creating) {
        if (CreateConflictStatus(http_status)) {
          credential_phase_ = CredentialPhase::FinalDerivingAfterCreate;
          post_create_probe_reason_ = PostCreateProbeReason::CreateRejected;
          std::fprintf(stderr,
                       "OMS polymarket credential stage=create conflict "
                       "http_status=%u probing derive\n",
                       static_cast<unsigned>(http_status));
          const AdapterResult started = BeginCredentialDerive();
          if (started != AdapterResult::Ok) {
            if (FailCredential("credential_create_rejected", http_status,
                               now_ns) != AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
          }
          continue;
        }
        if (CredentialHttpTemporary(http_status)) {
          credential_phase_ = CredentialPhase::FinalDerivingAfterCreate;
          post_create_probe_reason_ = PostCreateProbeReason::CreateAmbiguous;
          std::fprintf(stderr,
                       "OMS polymarket credential stage=create ambiguous "
                       "http_status=%u probing derive\n",
                       static_cast<unsigned>(http_status));
          const AdapterResult started = BeginCredentialDerive();
          if (started != AdapterResult::Ok) {
            if (FailCredential("credential_create_rejected", http_status,
                               now_ns) != AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
          }
          continue;
        }
        if (FailCredential("credential_create_rejected", http_status, now_ns) !=
            AdapterResult::Ok) {
          result.result = AdapterResult::WouldBlock;
          return result;
        }
        continue;
      }
      if (CredentialHttpTemporary(http_status)) {
        ScheduleCredentialRetry(now_ns);
        continue;
      }
      if (DeriveExplicitlyMissing(http_status, transport_event.payload)) {
        std::fprintf(stderr,
                     "OMS polymarket credential stage=derive missing "
                     "http_status=%u\n",
                     static_cast<unsigned>(http_status));
        if (auth_refresh_attempted_ &&
            credential_phase_ == CredentialPhase::Deriving) {
          if (FailCredential("credential_not_found", http_status, now_ns) !=
              AdapterResult::Ok) {
            result.result = AdapterResult::WouldBlock;
            return result;
          }
          continue;
        }
        if (credential_phase_ == CredentialPhase::FinalDerivingAfterCreate) {
          if (post_create_probe_reason_ ==
              PostCreateProbeReason::CreateRejected) {
            if (FailCredential("credential_not_found", http_status, now_ns) !=
                AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
            continue;
          }
          credential_phase_ = CredentialPhase::Creating;
          post_create_probe_reason_ = PostCreateProbeReason::None;
          ScheduleCredentialRetry(now_ns);
          continue;
        }
        const AdapterResult started = BeginCredentialCreate();
        if (started != AdapterResult::Ok) {
          if (FailCredential("credential_create_rejected", http_status,
                             now_ns) != AdapterResult::Ok) {
            result.result = AdapterResult::WouldBlock;
            return result;
          }
        }
        continue;
      }
      if (FailCredential("credential_invalid_l1_headers", http_status,
                         now_ns) != AdapterResult::Ok) {
        result.result = AdapterResult::WouldBlock;
        return result;
      }
      continue;
    }

    if (status_ == AdapterStatus::Authenticating &&
        (transport_event.kind == TransportEventKind::SessionReady ||
         transport_event.kind == TransportEventKind::SessionLost))
      continue;

    if (transport_event.kind == TransportEventKind::SessionReady ||
        transport_event.kind == TransportEventKind::SessionLost) {
      const bool ready =
          transport_event.kind == TransportEventKind::SessionReady;
      AdapterResult status_reason =
          ready ? AdapterResult::Ok : AdapterResult::NotReady;
      if (ready) {
        session_ever_ready_ = true;
        std::fprintf(stderr, "OMS polymarket user websocket connected\n");
      }
      if (!ready) {
        reconnect_reconcile_required_ = session_ever_ready_;
        reconcile_request_id_ = 0;
        reconcile_deadline_ns_ = 0;
        heartbeat_due_ns_ = 0;
        if (reconcile_generation_ != 0) {
          AdapterEvent interrupted =
              Event(identity(), AdapterEventKind::ReconcileComplete);
          interrupted.reconcile.generation = reconcile_generation_;
          interrupted.reconcile.result = AdapterResult::Failed;
          reconcile_generation_ = 0;
          if (queue(interrupted) != AdapterResult::Ok) {
            result.result = AdapterResult::WouldBlock;
            return result;
          }
        }
        if (transport_event.auth_rejected) {
          std::fprintf(stderr,
                       "OMS polymarket user websocket authentication failed "
                       "close_code=1008\n");
          if (auth_refresh_attempted_) {
            if (FailCredential("user_websocket_auth_rejected", 1008, now_ns) !=
                AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
            continue;
          }
          auth_refresh_attempted_ = true;
          reconnect_due_ns_ = 0;
          reconnect_delay_ns_ = config_.reconnect_initial_ns;
          config_.transport->close();
          ClearDerivedCredentials();
          WipeUserSubscription();
          status_ = AdapterStatus::Authenticating;
          resume_status_ = status_;
          const AdapterResult started = BeginCredentialDerive();
          if (started != AdapterResult::Ok) {
            status_ = AdapterStatus::Failed;
            resume_status_ = status_;
            status_reason = started;
          }
        } else {
          reconnect_delay_ns_ = config_.reconnect_initial_ns;
          reconnect_due_ns_ = SaturatingAdd(now_ns, reconnect_delay_ns_);
          status_ = AdapterStatus::Reconnecting;
        }
      } else if (status_ == AdapterStatus::Connecting ||
                 status_ == AdapterStatus::Reconnecting ||
                 (status_ == AdapterStatus::Backpressured &&
                  (resume_status_ == AdapterStatus::Connecting ||
                   resume_status_ == AdapterStatus::Reconnecting))) {
        status_reason = StartInternalReconcile(now_ns);
      }
      resume_status_ = status_;
      AdapterEvent status_event = Event(identity(), AdapterEventKind::Status);
      status_event.status.identity = identity();
      status_event.status.status = status_;
      status_event.status.reason = status_reason;
      status_event.status.event_time_ns = now_ns;
      if (queue(status_event) != AdapterResult::Ok) {
        result.result = AdapterResult::WouldBlock;
        return result;
      }
      continue;
    }

    if (transport_event.kind == TransportEventKind::UserMessage) {
      std::array<api::VenueEvent, kMaximumTradeEvents> parsed_events{};
      std::size_t parsed_count = 0;
      const ProtocolResult parsed = ParseUserMessageEvents(
          transport_event.payload, parsed_events.data(), parsed_events.size(),
          parsed_count);
      if (parsed == ProtocolResult::Unsupported) continue;
      if (parsed != ProtocolResult::Ok) {
        result.result = AdapterResult::Failed;
        return result;
      }
      for (std::size_t parsed_index = 0; parsed_index < parsed_count;
           ++parsed_index) {
        AdapterEvent venue_event = Event(identity(), AdapterEventKind::Venue);
        venue_event.venue = parsed_events[parsed_index];
        const std::string_view venue_id =
            IdView(venue_event.venue.venue_order_id);
        if (!venue_id.empty()) {
          for (OrderBinding& binding : orders_) {
            if (binding.used && IdView(binding.venue_order_id) == venue_id) {
              venue_event.venue.handle = binding.handle;
              if (venue_event.venue.type == api::VenueEventType::CancelAck ||
                  venue_event.venue.type ==
                      api::VenueEventType::ReconcileTerminal)
                binding.used = false;
              break;
            }
          }
        }
        if (queue(venue_event) != AdapterResult::Ok) {
          result.result = AdapterResult::WouldBlock;
          return result;
        }
      }
      continue;
    }

    if (transport_event.kind != TransportEventKind::HttpResponse) {
      result.result = AdapterResult::Failed;
      return result;
    }

    if (query_.active &&
        transport_event.request_id == query_.request_id) {
      query_.request_id = 0;
      query_.deadline_ns = 0;
      AdapterResult query_result = AdapterResult::Ok;
      if (transport_event.status_code >= 400 ||
          transport_event.status_code == 0) {
        query_result = AdapterResult::Failed;
      } else if (query_.kind == api::QueryKind::OpenOrders) {
        std::array<OpenOrderSnapshot, kMaximumOpenOrders> parsed_items{};
        std::size_t parsed_count = 0;
        const ProtocolResult parsed = ParseOpenOrderSnapshots(
            transport_event.payload, query_.pagination, parsed_items.data(),
            parsed_items.size(), parsed_count);
        if (parsed != ProtocolResult::Ok) {
          query_result = ProtocolAdapterResult(parsed);
        } else {
          for (std::size_t index = 0; index < parsed_count; ++index) {
            const auto& source = parsed_items[index];
            const auto& requested = query_.request.request;
            if (requested.scope == api::QueryScope::SingleInstrument &&
                (requested.instrument.kind !=
                     api::ExecutionRouteKind::Polymarket ||
                 requested.instrument.polymarket.token_id !=
                     source.token_id)) {
              continue;
            }
            AdapterEvent item =
                Event(identity(), AdapterEventKind::OpenOrderSnapshot);
            item.open_order.query_token = query_.request.token;
            item.open_order.account_id = query_.request.request.account_id;
            item.open_order.instrument_id =
                config_.resolve_polymarket_token(
                    config_.instrument_context, source.token_id);
            if (item.open_order.instrument_id == 0)
              item.open_order.reserved |= api::SnapshotUnmapped;
            item.open_order.venue_order_id = source.venue_order_id;
            item.open_order.side = source.side;
            item.open_order.status = source.status;
            item.open_order.quantity = source.quantity;
            item.open_order.price = source.price;
            item.open_order.matched_quantity = source.matched_quantity;
            const std::uint8_t scale =
                std::max(source.quantity.scale,
                         source.matched_quantity.scale);
            std::uint64_t quantity = 0, matched = 0;
            if (!AtScale(source.quantity, scale, quantity) ||
                (source.matched_quantity.value != 0 &&
                 !AtScale(source.matched_quantity, scale, matched)) ||
                matched > quantity) {
              query_result = AdapterResult::InvalidArgument;
              break;
            }
            item.open_order.remaining_quantity.value =
                static_cast<std::int64_t>(quantity - matched);
            item.open_order.remaining_quantity.scale = scale;
            if (queue(item) != AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
          }
          if (query_result == AdapterResult::Ok &&
              !query_.pagination.complete) {
            query_result = submit_query_page();
            if (query_result == AdapterResult::Ok) continue;
          }
        }
      } else {
        std::array<PositionSnapshot, kMaximumOpenOrders> parsed_items{};
        std::size_t parsed_count = 0;
        const ProtocolResult parsed =
            ParsePositions(transport_event.payload, parsed_items.data(),
                           parsed_items.size(), parsed_count);
        if (parsed != ProtocolResult::Ok) {
          query_result = ProtocolAdapterResult(parsed);
        } else {
          for (std::size_t index = 0; index < parsed_count; ++index) {
            const auto& requested = query_.request.request;
            if (requested.scope == api::QueryScope::SingleInstrument &&
                (requested.instrument.kind !=
                     api::ExecutionRouteKind::Polymarket ||
                 requested.instrument.polymarket.token_id !=
                     parsed_items[index].token_id)) {
              continue;
            }
            AdapterEvent item =
                Event(identity(), AdapterEventKind::PositionSnapshot);
            item.position.query_token = query_.request.token;
            item.position.account_id = query_.request.request.account_id;
            item.position.instrument_id =
                config_.resolve_polymarket_token(
                    config_.instrument_context, parsed_items[index].token_id);
            item.position.quantity = parsed_items[index].quantity;
            if (item.position.instrument_id == 0)
              item.position.reserved |= api::SnapshotUnmapped;
            if (queue(item) != AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
          }
        }
      }
      AdapterEvent complete =
          Event(identity(), AdapterEventKind::QueryComplete);
      complete.query_complete.query_token = query_.request.token;
      complete.query_complete.account_id =
          query_.request.request.account_id;
      complete.query_complete.kind = query_.kind;
      complete.query_complete.error =
          query_result == AdapterResult::Ok
              ? api::Error::Ok
              : (query_result == AdapterResult::Unsupported
                     ? api::Error::Unsupported
                     : api::Error::NotReady);
      query_ = {};
      if (queue(complete) != AdapterResult::Ok) {
        result.result = AdapterResult::WouldBlock;
        return result;
      }
      continue;
    }

    if (transport_event.request_id == reconcile_request_id_ &&
        reconcile_generation_ != 0) {
      reconcile_request_id_ = 0;
      reconcile_deadline_ns_ = 0;
      AdapterResult reconcile_result = AdapterResult::Ok;
      if (transport_event.status_code >= 400) {
        std::fprintf(stderr,
                     "OMS polymarket open_orders_reconcile_http_failed "
                     "http_status=%u\n",
                     static_cast<unsigned>(transport_event.status_code));
        reconcile_result = AdapterResult::Failed;
      } else {
        std::array<api::VenueEvent, kOrderCapacity> parsed_events{};
        std::size_t parsed_count = 0;
        const ProtocolResult parsed = ParseOpenOrdersPage(
            transport_event.payload, pagination_, parsed_events.data(),
            parsed_events.size(), parsed_count);
        if (parsed != ProtocolResult::Ok) {
          std::fprintf(stderr,
                       "OMS polymarket open_orders_reconcile_parse_failed\n");
          reconcile_result = AdapterResult::Failed;
        } else {
          for (std::size_t index = 0; index < parsed_count; ++index) {
            AdapterEvent venue_event =
                Event(identity(), AdapterEventKind::Venue);
            venue_event.venue = parsed_events[index];
            const std::string_view venue_id =
                IdView(venue_event.venue.venue_order_id);
            bool local = false;
            for (OrderBinding& binding : orders_) {
              if (binding.used && IdView(binding.venue_order_id) == venue_id) {
                venue_event.venue.handle = binding.handle;
                binding.reconcile_seen = true;
                local = true;
                break;
              }
            }
            if (!local) reconcile_uncertain_ = true;
            if (queue(venue_event) != AdapterResult::Ok) {
              result.result = AdapterResult::WouldBlock;
              return result;
            }
          }
          if (!pagination_.complete) {
            reconcile_result = submit_reconcile_page();
            if (reconcile_result == AdapterResult::Ok) continue;
          }
        }
      }
      AdapterEvent complete =
          Event(identity(), AdapterEventKind::ReconcileComplete);
      complete.reconcile.generation = reconcile_generation_;
      if (reconcile_result == AdapterResult::Ok) {
        for (const OrderBinding& binding : orders_)
          if (binding.used && !binding.reconcile_seen)
            reconcile_uncertain_ = true;
      }
      complete.reconcile.result =
          reconcile_result == AdapterResult::Ok && reconcile_uncertain_
              ? AdapterResult::Failed
              : reconcile_result;
      reconcile_generation_ = 0;
      status_ = reconcile_result == AdapterResult::Ok ? AdapterStatus::Ready
                                                       : AdapterStatus::Failed;
      resume_status_ = status_;
      if (reconcile_result == AdapterResult::Ok) {
        reconnect_reconcile_required_ = false;
        if (status_ == AdapterStatus::Ready)
          std::fprintf(stderr,
                       "OMS polymarket initial reconcile completed\n");
      }
      if (queue(complete) != AdapterResult::Ok) {
        result.result = AdapterResult::WouldBlock;
        return result;
      }
      continue;
    }

    CommandSlot* slot = find_request(transport_event.request_id);
    if (slot == nullptr) continue;
    const std::size_t slot_index =
        static_cast<std::size_t>(slot - commands_.data());
    AdapterEvent command_event =
        Event(identity(), AdapterEventKind::CommandResult);
    command_event.command_result.command_id = slot->command_id;
    command_event.command_result.kind = slot->kind;
    command_event.command_result.request_token = slot->token;
    command_event.command_result.result = AdapterResult::Failed;
    AdapterEvent venue_event = Event(identity(), AdapterEventKind::Venue);
    ProtocolResult parsed = ProtocolResult::Malformed;
    if (transport_event.status_code < 400) {
      parsed =
          slot->kind == AdapterCommandKind::Place
              ? ParsePlaceResponse(transport_event.payload, slot->handle,
                                   slot->token, venue_event.venue)
              : ParseCancelResponse(transport_event.payload,
                                    IdView(slot->venue_order_id), slot->handle,
                                    slot->token, venue_event.venue);
    } else if (transport_event.status_code < 500) {
      venue_event.venue.handle = slot->handle;
      venue_event.venue.token = slot->token;
      if (slot->kind == AdapterCommandKind::Place) {
        venue_event.venue.type = api::VenueEventType::NewReject;
      } else {
        venue_event.venue.type = api::VenueEventType::CancelReject;
        venue_event.venue.venue_order_id = slot->venue_order_id;
      }
      parsed = ProtocolResult::Ok;
    }
    if (parsed == ProtocolResult::Ok) {
      command_event.command_result.result =
          transport_event.status_code < 400 ? AdapterResult::Ok
                                            : AdapterResult::Failed;
      if (transport_event.status_code < 400 &&
          slot->kind == AdapterCommandKind::Place &&
          venue_event.venue.type == api::VenueEventType::NewAck)
        bind_order(slot->handle, venue_event.venue.venue_order_id,
                   slot->instrument_id);
      if (transport_event.status_code < 400 &&
          slot->kind == AdapterCommandKind::Cancel &&
          venue_event.venue.type == api::VenueEventType::CancelAck) {
        for (OrderBinding& binding : orders_)
          if (binding.used && binding.handle == slot->handle)
            binding.used = false;
      }
    }
    if (queue(command_event) != AdapterResult::Ok ||
        (parsed == ProtocolResult::Ok && queue(venue_event) != AdapterResult::Ok)) {
      result.result = AdapterResult::WouldBlock;
      return result;
    }
    release(slot_index);
  }
  return result;
}

AdapterResult PolymarketTradeAdapter::on_deadline(
    const AdapterDeadline& deadline, std::uint64_t now_ns,
    const AdapterEventSink& sink) noexcept {
  if (deadline.due_time_ns > now_ns) return AdapterResult::InvalidArgument;
  if (deadline.kind == AdapterDeadlineKind::Heartbeat) {
    const bool ready =
        status_ == AdapterStatus::Ready ||
        (status_ == AdapterStatus::Backpressured &&
         resume_status_ == AdapterStatus::Ready);
    if (!ready) return AdapterResult::NotReady;
    if (heartbeat_due_ns_ == 0 || heartbeat_due_ns_ > now_ns)
      return AdapterResult::StaleReservation;
    const AdapterResult result = config_.transport->send_heartbeat();
    if (result == AdapterResult::Ok) {
      heartbeat_due_ns_ =
          now_ns > std::numeric_limits<std::uint64_t>::max() -
                       config_.heartbeat_interval_ns
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + config_.heartbeat_interval_ns;
    }
    return result;
  }
  if (deadline.kind == AdapterDeadlineKind::Reconnect) {
    const bool reconnecting =
        status_ == AdapterStatus::Reconnecting ||
        (status_ == AdapterStatus::Backpressured &&
         resume_status_ == AdapterStatus::Reconnecting);
    if (!reconnecting) return AdapterResult::NotReady;
    if (reconnect_due_ns_ == 0 || reconnect_due_ns_ > now_ns)
      return AdapterResult::WouldBlock;
    const AdapterResult result = config_.transport->request_reconnect();
    if (result == AdapterResult::Ok) {
      const std::uint64_t delay = reconnect_delay_ns_;
      const std::uint64_t next_delay =
          delay >= config_.reconnect_max_ns - delay
              ? config_.reconnect_max_ns
              : std::min(delay * 2U, config_.reconnect_max_ns);
      reconnect_due_ns_ =
          now_ns > std::numeric_limits<std::uint64_t>::max() - next_delay
              ? std::numeric_limits<std::uint64_t>::max()
              : now_ns + next_delay;
      reconnect_delay_ns_ = next_delay;
    }
    return result;
  }
  if (deadline.kind != AdapterDeadlineKind::Request)
    return AdapterResult::Unsupported;
  const bool query_due =
      query_.active && query_.request_id != 0 && query_.deadline_ns != 0 &&
      query_.deadline_ns <= now_ns &&
      (deadline.id == 0 || deadline.id == query_.request_id);
  if (query_due) {
    AdapterEvent complete = Event(identity(), AdapterEventKind::QueryComplete);
    complete.query_complete.query_token = query_.request.token;
    complete.query_complete.account_id =
        query_.request.request.account_id;
    complete.query_complete.kind = query_.kind;
    complete.query_complete.error = api::Error::NotReady;
    query_ = {};
    return offer(sink, complete);
  }
  const bool reconcile_due =
      reconcile_request_id_ != 0 && reconcile_generation_ != 0 &&
      reconcile_deadline_ns_ != 0 && reconcile_deadline_ns_ <= now_ns &&
      (deadline.id == 0 || deadline.id == reconcile_request_id_) &&
      (deadline.generation == 0 ||
       deadline.generation == reconcile_generation_);
  CommandSlot* slot = nullptr;
  if (deadline.id == 0) {
    for (CommandSlot& candidate : commands_) {
      if (candidate.inflight && candidate.deadline_ns != 0 &&
          candidate.deadline_ns <= now_ns &&
          (slot == nullptr || candidate.deadline_ns < slot->deadline_ns))
        slot = &candidate;
    }
  } else {
    slot = find_request(deadline.id);
    if (slot != nullptr && deadline.generation != 0 &&
        slot->generation != deadline.generation)
      slot = nullptr;
    if (slot != nullptr &&
        (slot->deadline_ns == 0 || slot->deadline_ns > now_ns))
      slot = nullptr;
  }
  if (reconcile_due &&
      (slot == nullptr || reconcile_deadline_ns_ <= slot->deadline_ns)) {
    AdapterEvent complete =
        Event(identity(), AdapterEventKind::ReconcileComplete);
    complete.reconcile.generation = reconcile_generation_;
    complete.reconcile.result = AdapterResult::Failed;
    reconcile_generation_ = 0;
    reconcile_request_id_ = 0;
    reconcile_deadline_ns_ = 0;
    status_ = AdapterStatus::Failed;
    resume_status_ = status_;
    return offer(sink, complete);
  }
  if (slot == nullptr) return AdapterResult::StaleReservation;
  const std::size_t index = static_cast<std::size_t>(slot - commands_.data());
  AdapterEvent event = Event(identity(), AdapterEventKind::CommandResult);
  event.command_result.command_id = slot->command_id;
  event.command_result.kind = slot->kind;
  event.command_result.request_token = slot->token;
  event.command_result.result = AdapterResult::Failed;
  release(index);
  return offer(sink, event);
}

AdapterResult PolymarketTradeAdapter::begin_reconcile(
    std::uint64_t generation, std::uint64_t now_ns,
    const AdapterEventSink& sink) noexcept {
  (void)now_ns;
  (void)sink;
  if (generation == 0 || reconcile_generation_ != 0 ||
      (status_ != AdapterStatus::Ready &&
       status_ != AdapterStatus::Backpressured))
    return AdapterResult::InvalidArgument;
  pagination_ = {};
  reconcile_uncertain_ = false;
  for (OrderBinding& binding : orders_)
    if (binding.used) binding.reconcile_seen = false;
  reconcile_generation_ = generation;
  status_ = AdapterStatus::Reconciling;
  resume_status_ = status_;
  const AdapterResult result = submit_reconcile_page();
  if (result != AdapterResult::Ok) {
    reconcile_generation_ = 0;
    status_ = AdapterStatus::Failed;
    resume_status_ = status_;
  }
  return result;
}

AdapterResult PolymarketTradeAdapter::shutdown(
    std::uint64_t now_ns, const AdapterEventSink& sink) noexcept {
  ClearDerivedCredentials();
  WipeUserSubscription();
  if (config_.transport != nullptr) config_.transport->close();
  if (config_.data_transport != nullptr) config_.data_transport->close();
  status_ = AdapterStatus::Stopped;
  resume_status_ = status_;
  AdapterEvent event = Event(identity(), AdapterEventKind::Status);
  event.status.identity = identity();
  event.status.status = status_;
  event.status.reason = AdapterResult::Ok;
  event.status.event_time_ns = now_ns;
  return offer(sink, event);
}

}  // namespace oms::exchange::polymarket
