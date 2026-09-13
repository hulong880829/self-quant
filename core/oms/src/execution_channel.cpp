#include "oms/api/execution_channel.h"

#include <algorithm>
#include <array>
#include <charconv>
#include <chrono>
#include <cctype>
#include <cstring>
#include <limits>
#include <string>
#include <string_view>
#include <utility>

#include <openssl/crypto.h>

#include "net/tls_websocket.h"
#include "oms/exchange/binance/trade_adapter.h"
#include "oms/exchange/live_transport.h"
#include "oms/exchange/polymarket/trade_adapter.h"

namespace oms::api {
namespace {

using exchange::AsyncIoDriver;
using exchange::TradeAdapter;
using exchange::binance::BinanceAdapterConfig;
using exchange::binance::BinanceTradeAdapter;
using exchange::binance::Product;
using exchange::live::Config;
using exchange::live::Endpoint;
using exchange::live::LiveTransport;
using exchange::polymarket::AdapterConfig;
using exchange::polymarket::Credentials;
using exchange::polymarket::PolymarketTradeAdapter;

struct EndpointDefaults {
  std::string_view rest_host;
  std::string_view rest_service;
  std::string_view websocket_host;
  std::string_view websocket_service;
  std::string_view trading_websocket_host;
  std::string_view trading_websocket_service;
};

constexpr EndpointDefaults kBinanceSpotEndpoints{
    "api.binance.com", "443", "stream.binance.com", "9443",
    "ws-api.binance.com", "443"};
constexpr EndpointDefaults kBinanceUsdmEndpoints{
    "fapi.binance.com", "443", "fstream.binance.com", "443",
    "ws-fapi.binance.com", "443"};
constexpr EndpointDefaults kPolymarketEndpoints{
    "clob.polymarket.com", "443", "ws-subscriptions-clob.polymarket.com",
    "443", {}, {}};
constexpr std::string_view kPolymarketDataHost = "data-api.polymarket.com";
constexpr std::string_view kPolymarketDataService = "443";

class SecretText {
 public:
  SecretText() = default;
  ~SecretText() noexcept { clear(); }
  SecretText(const SecretText&) = delete;
  SecretText& operator=(const SecretText&) = delete;

  void assign(std::string_view value) { value_.assign(value); }
  [[nodiscard]] std::string_view view() const noexcept { return value_; }

 private:
  void clear() noexcept {
    if (!value_.empty()) OPENSSL_cleanse(value_.data(), value_.size());
    value_.clear();
  }
  std::string value_;
};

struct BinanceSecrets {
  SecretText api_key;
  SecretText secret_key;
};

struct PolymarketSecrets {
  SecretText signer_address;
  SecretText funder_address;
  SecretText private_key;
  SecretText api_key;
  SecretText api_secret;
  SecretText passphrase;
};

struct SecretStore {
  BinanceSecrets spot;
  BinanceSecrets usdm;
  PolymarketSecrets polymarket;
};

bool HasControl(std::string_view value) noexcept {
  return std::any_of(value.begin(), value.end(), [](char value_in) {
    return static_cast<unsigned char>(value_in) < 0x20U ||
           static_cast<unsigned char>(value_in) == 0x7fU;
  });
}

bool ValidSecret(std::string_view value) noexcept {
  return !value.empty() && !HasControl(value);
}

bool ValidHost(std::string_view host) noexcept {
  if (host.empty() || host.find("://") != std::string_view::npos)
    return false;
  return std::none_of(host.begin(), host.end(), [](char value) {
    return std::isspace(static_cast<unsigned char>(value)) != 0 ||
           value == '/' || value == '?' || value == '#';
  });
}

bool ValidService(std::string_view service) noexcept {
  if (service.empty()) return false;
  unsigned port = 0;
  const auto converted =
      std::from_chars(service.data(), service.data() + service.size(), port);
  return converted.ec == std::errc{} &&
         converted.ptr == service.data() + service.size() && port != 0 &&
         port <= 65535;
}

bool MakeTransportConfig(const VenueEndpointOverrides& overrides,
                         const EndpointDefaults& defaults,
                         Config& output) {
  const std::string_view rest_host =
      overrides.rest.host.empty() ? defaults.rest_host : overrides.rest.host;
  const std::string_view rest_service = overrides.rest.service.empty()
                                            ? defaults.rest_service
                                            : overrides.rest.service;
  const std::string_view websocket_host =
      overrides.websocket.host.empty() ? defaults.websocket_host
                                       : overrides.websocket.host;
  const std::string_view websocket_service =
      overrides.websocket.service.empty() ? defaults.websocket_service
                                          : overrides.websocket.service;
  if (!ValidHost(rest_host) || !ValidService(rest_service) ||
      !ValidHost(websocket_host) || !ValidService(websocket_service)) {
    return false;
  }
  output.rest = Endpoint{std::string(rest_host), std::string(rest_service)};
  output.websocket =
      Endpoint{std::string(websocket_host), std::string(websocket_service)};
  return true;
}

bool MakeBinanceTradingConfig(const BinanceExecutionConfig& config,
                              const EndpointDefaults& defaults,
                              const Config& control,
                              Config& output) {
  const std::string_view host =
      config.trading_websocket.host.empty()
          ? defaults.trading_websocket_host
          : config.trading_websocket.host;
  const std::string_view service =
      config.trading_websocket.service.empty()
          ? defaults.trading_websocket_service
          : config.trading_websocket.service;
  if (!ValidHost(host) || !ValidService(service)) return false;
  output = control;
  output.websocket = Endpoint{std::string(host), std::string(service)};
  output.request_slots = 1;
  return true;
}

bool CopyBinanceCredentials(const BinanceExecutionConfig& config,
                            BinanceSecrets& output) {
  BinanceCredentialView source = config.credentials;
  if (config.credential_provider != nullptr &&
      !config.credential_provider(config.credential_context, source)) {
    return false;
  }
  if (!ValidSecret(source.api_key) || !ValidSecret(source.secret_key))
    return false;
  output.api_key.assign(source.api_key);
  output.secret_key.assign(source.secret_key);
  return true;
}

bool CopyPolymarketCredentials(const PolymarketExecutionConfig& config,
                               PolymarketSecrets& output) {
  PolymarketCredentialView source = config.credentials;
  if (config.credential_provider != nullptr &&
      !config.credential_provider(config.credential_context, source)) {
    return false;
  }
  if (!ValidSecret(source.signer_address) ||
      !ValidSecret(source.funder_address) ||
      !ValidSecret(source.private_key) || !ValidSecret(source.api_key) ||
      !ValidSecret(source.api_secret) || !ValidSecret(source.passphrase)) {
    return false;
  }
  output.signer_address.assign(source.signer_address);
  output.funder_address.assign(source.funder_address);
  output.private_key.assign(source.private_key);
  output.api_key.assign(source.api_key);
  output.api_secret.assign(source.api_secret);
  output.passphrase.assign(source.passphrase);
  return true;
}

void AppendJsonString(std::string_view source, std::string& output) {
  constexpr char hex[] = "0123456789abcdef";
  output.push_back('"');
  for (const char raw : source) {
    const unsigned char value = static_cast<unsigned char>(raw);
    switch (value) {
      case '"':
        output.append("\\\"");
        break;
      case '\\':
        output.append("\\\\");
        break;
      case '\b':
        output.append("\\b");
        break;
      case '\f':
        output.append("\\f");
        break;
      case '\n':
        output.append("\\n");
        break;
      case '\r':
        output.append("\\r");
        break;
      case '\t':
        output.append("\\t");
        break;
      default:
        if (value < 0x20U) {
          output.append("\\u00");
          output.push_back(hex[value >> 4U]);
          output.push_back(hex[value & 0x0fU]);
        } else {
          output.push_back(static_cast<char>(value));
        }
        break;
    }
  }
  output.push_back('"');
}

std::string PolymarketSubscription(const PolymarketSecrets& credentials) {
  std::string output;
  output.reserve(credentials.api_key.view().size() +
                 credentials.api_secret.view().size() +
                 credentials.passphrase.view().size() + 96);
  output.append("{\"auth\":{\"apiKey\":");
  AppendJsonString(credentials.api_key.view(), output);
  output.append(",\"secret\":");
  AppendJsonString(credentials.api_secret.view(), output);
  output.append(",\"passphrase\":");
  AppendJsonString(credentials.passphrase.view(), output);
  output.append("},\"type\":\"USER\"}");
  return output;
}

struct BinanceResolver {
  // BinanceTradeAdapter consults its adapter-owned order bindings first.
  // This callback is deliberately a safe miss for stale/unbound handles.
  static bool ResolveCancel(
      void*, OrderHandle,
      exchange::binance::ResolvedCancel&) noexcept {
    return false;
  }
};

std::uint64_t WallClockMilliseconds(void*) noexcept {
  const auto now = std::chrono::system_clock::now().time_since_epoch();
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::milliseconds>(now).count());
}

bool CopyPolymarketAdapterCredentials(const PolymarketSecrets& source,
                                      Credentials& output) noexcept {
  return output.signer_address.assign(source.signer_address.view()) &&
         output.funder_address.assign(source.funder_address.view()) &&
         output.private_key.assign(source.private_key.view()) &&
         output.api_key.assign(source.api_key.view()) &&
         output.api_secret.assign(source.api_secret.view()) &&
         output.passphrase.assign(source.passphrase.view());
}

}  // namespace

struct ExecutionChannel::Impl {
  // Destruction is reverse declaration order: OmsApi stops first, then trade
  // adapters, transport wrappers, transports, SSL, and secrets.
  static InstrumentId ResolvePolymarketToken(
      void* context,
      const std::array<std::uint8_t, 32>& token_id) noexcept {
    auto* self = static_cast<Impl*>(context);
    return self == nullptr || self->api == nullptr
               ? 0
               : self->api->resolve_polymarket_token(token_id);
  }

  SecretStore secrets;
  net::SharedSslContext ssl_context;
  std::unique_ptr<LiveTransport> spot_transport;
  std::unique_ptr<LiveTransport> usdm_transport;
  std::unique_ptr<LiveTransport> spot_trading_transport;
  std::unique_ptr<LiveTransport> usdm_trading_transport;
  std::unique_ptr<LiveTransport> polymarket_transport;
  std::unique_ptr<LiveTransport> polymarket_data_transport;
  std::unique_ptr<LiveTransport::BinanceAdapter> spot_wrapper;
  std::unique_ptr<LiveTransport::BinanceAdapter> usdm_wrapper;
  std::unique_ptr<LiveTransport::PolymarketAdapter> polymarket_wrapper;
  std::unique_ptr<LiveTransport::PolymarketAdapter> polymarket_data_wrapper;
  std::unique_ptr<BinanceTradeAdapter> spot_adapter;
  std::unique_ptr<BinanceTradeAdapter> usdm_adapter;
  std::unique_ptr<PolymarketTradeAdapter> polymarket_adapter;
  std::unique_ptr<OmsApi> api;
};

ExecutionChannel::ExecutionChannel(std::unique_ptr<Impl> impl) noexcept
    : impl_(std::move(impl)) {}

ExecutionChannel::~ExecutionChannel() = default;

Result<std::unique_ptr<ExecutionChannel>> ExecutionChannel::Create(
    const RuntimeConfig& config, std::span<const InstrumentInit> instruments,
    std::span<const ReplayStep> replay) {
  auto created = OmsApi::Create(config, instruments, replay);
  if (!created) return {{}, created.error};
  auto impl = std::make_unique<Impl>();
  impl->api = std::move(created.value);
  return {std::unique_ptr<ExecutionChannel>(
              new ExecutionChannel(std::move(impl))),
          Error::Ok};
}

Result<std::unique_ptr<ExecutionChannel>> ExecutionChannel::Create(
    const ExecutionChannelConfig& config,
    std::span<const InstrumentInit> instruments) {
  const std::size_t adapter_count =
      static_cast<std::size_t>(config.binance_spot.enabled) +
      static_cast<std::size_t>(config.binance_usdm.enabled) +
      static_cast<std::size_t>(config.polymarket.enabled);
  if (adapter_count == 0 ||
      adapter_count > exchange::kMaxTradeAdapters ||
      config.event_budget == 0 || instruments.empty() ||
      config.socket.receive_buffer_bytes < 0 ||
      config.socket.send_buffer_bytes < 0 ||
      config.socket.busy_poll_us < 0) {
    return {{}, Error::InvalidArgument};
  }

  try {
    auto impl = std::make_unique<Impl>();

    Config spot_transport_config{};
    Config usdm_transport_config{};
    Config spot_trading_transport_config{};
    Config usdm_trading_transport_config{};
    Config polymarket_transport_config{};
    Config polymarket_data_transport_config{};
    const net::SocketOptions socket_options{
        config.socket.tcp_nodelay,
        config.socket.receive_buffer_bytes,
        config.socket.send_buffer_bytes,
        config.socket.busy_poll_us};
    spot_transport_config.socket_options = socket_options;
    usdm_transport_config.socket_options = socket_options;
    spot_trading_transport_config.socket_options = socket_options;
    usdm_trading_transport_config.socket_options = socket_options;
    polymarket_transport_config.socket_options = socket_options;
    polymarket_data_transport_config.socket_options = socket_options;
    polymarket_data_transport_config.rest = {
        config.polymarket.data_api.host.empty()
            ? std::string(kPolymarketDataHost)
            : std::string(config.polymarket.data_api.host),
        config.polymarket.data_api.service.empty()
            ? std::string(kPolymarketDataService)
            : std::string(config.polymarket.data_api.service)};
    polymarket_data_transport_config.websocket =
        polymarket_data_transport_config.rest;
    if ((config.binance_spot.enabled &&
         (config.binance_spot.account_id == 0 ||
          !MakeTransportConfig(config.binance_spot.endpoints,
                               kBinanceSpotEndpoints,
                               spot_transport_config) ||
          !MakeBinanceTradingConfig(config.binance_spot,
                                    kBinanceSpotEndpoints,
                                    spot_transport_config,
                                    spot_trading_transport_config) ||
          !CopyBinanceCredentials(config.binance_spot,
                                  impl->secrets.spot))) ||
        (config.binance_usdm.enabled &&
         (config.binance_usdm.account_id == 0 ||
          !MakeTransportConfig(config.binance_usdm.endpoints,
                               kBinanceUsdmEndpoints,
                               usdm_transport_config) ||
          !MakeBinanceTradingConfig(config.binance_usdm,
                                    kBinanceUsdmEndpoints,
                                    usdm_transport_config,
                                    usdm_trading_transport_config) ||
          !CopyBinanceCredentials(config.binance_usdm,
                                  impl->secrets.usdm))) ||
        (config.polymarket.enabled &&
         (!MakeTransportConfig(config.polymarket.endpoints,
                               kPolymarketEndpoints,
                               polymarket_transport_config) ||
          config.polymarket.account_id == 0 ||
          !ValidHost(polymarket_data_transport_config.rest.host) ||
          !ValidService(polymarket_data_transport_config.rest.service) ||
          !CopyPolymarketCredentials(config.polymarket,
                                     impl->secrets.polymarket)))) {
      return {{}, Error::InvalidArgument};
    }

    std::string ssl_error;
    impl->ssl_context = net::make_client_ssl_context(ssl_error);
    if (!impl->ssl_context) return {{}, Error::NotReady};

    std::array<TradeAdapter*, 3> adapters{};
    std::array<AsyncIoDriver*, 6> drivers{};
    std::array<AdapterRuntimeConfig::AccountRoute, 3> account_routes{};
    std::size_t adapter_index = 0;
    std::size_t driver_index = 0;

    if (config.binance_spot.enabled) {
      impl->spot_transport = std::make_unique<LiveTransport>(
          impl->ssl_context, std::move(spot_transport_config));
      impl->spot_trading_transport = std::make_unique<LiveTransport>(
          impl->ssl_context, std::move(spot_trading_transport_config));
      impl->spot_wrapper =
          std::make_unique<LiveTransport::BinanceAdapter>(
              *impl->spot_transport, *impl->spot_trading_transport,
              Product::Spot);
      BinanceAdapterConfig adapter_config{};
      adapter_config.product = Product::Spot;
      adapter_config.credentials = {
          impl->secrets.spot.api_key.view(),
          impl->secrets.spot.secret_key.view()};
      adapter_config.callbacks = {
          nullptr, &BinanceResolver::ResolveCancel, nullptr};
      adapter_config.transport = impl->spot_wrapper.get();
      impl->spot_adapter =
          std::make_unique<BinanceTradeAdapter>(adapter_config);
      if (impl->spot_adapter->status() == exchange::AdapterStatus::Failed)
        return {{}, Error::InvalidArgument};
      adapters[adapter_index++] = impl->spot_adapter.get();
      account_routes[adapter_index - 1] = {
          config.binance_spot.account_id,
          exchange::AdapterKind::BinanceSpot};
      drivers[driver_index++] = impl->spot_transport.get();
      drivers[driver_index++] = impl->spot_trading_transport.get();
    }

    if (config.binance_usdm.enabled) {
      impl->usdm_transport = std::make_unique<LiveTransport>(
          impl->ssl_context, std::move(usdm_transport_config));
      impl->usdm_trading_transport = std::make_unique<LiveTransport>(
          impl->ssl_context, std::move(usdm_trading_transport_config));
      impl->usdm_wrapper =
          std::make_unique<LiveTransport::BinanceAdapter>(
              *impl->usdm_transport, *impl->usdm_trading_transport,
              Product::Usdm);
      BinanceAdapterConfig adapter_config{};
      adapter_config.product = Product::Usdm;
      adapter_config.credentials = {
          impl->secrets.usdm.api_key.view(),
          impl->secrets.usdm.secret_key.view()};
      adapter_config.callbacks = {
          nullptr, &BinanceResolver::ResolveCancel, nullptr};
      adapter_config.transport = impl->usdm_wrapper.get();
      impl->usdm_adapter =
          std::make_unique<BinanceTradeAdapter>(adapter_config);
      if (impl->usdm_adapter->status() == exchange::AdapterStatus::Failed)
        return {{}, Error::InvalidArgument};
      adapters[adapter_index++] = impl->usdm_adapter.get();
      account_routes[adapter_index - 1] = {
          config.binance_usdm.account_id,
          exchange::AdapterKind::BinanceUsdm};
      drivers[driver_index++] = impl->usdm_transport.get();
      drivers[driver_index++] = impl->usdm_trading_transport.get();
    }

    if (config.polymarket.enabled) {
      impl->polymarket_transport = std::make_unique<LiveTransport>(
          impl->ssl_context, std::move(polymarket_transport_config));
      impl->polymarket_data_transport = std::make_unique<LiveTransport>(
          impl->ssl_context, std::move(polymarket_data_transport_config));
      impl->polymarket_wrapper =
          std::make_unique<LiveTransport::PolymarketAdapter>(
              *impl->polymarket_transport, std::chrono::seconds(5),
              "/ws/user",
              PolymarketSubscription(impl->secrets.polymarket));
      impl->polymarket_data_wrapper =
          std::make_unique<LiveTransport::PolymarketAdapter>(
              *impl->polymarket_data_transport, std::chrono::seconds(5), "",
              "");
      AdapterConfig adapter_config{};
      adapter_config.instrument_context = impl.get();
      adapter_config.resolve_polymarket_token =
          &ExecutionChannel::Impl::ResolvePolymarketToken;
      adapter_config.transport = impl->polymarket_wrapper.get();
      adapter_config.data_transport = impl->polymarket_data_wrapper.get();
      if (!CopyPolymarketAdapterCredentials(impl->secrets.polymarket,
                                            adapter_config.credentials)) {
        return {{}, Error::InvalidArgument};
      }
      adapter_config.now_ms = &WallClockMilliseconds;
      impl->polymarket_adapter =
          std::make_unique<PolymarketTradeAdapter>(adapter_config);
      if (impl->polymarket_adapter->status() ==
          exchange::AdapterStatus::Failed) {
        return {{}, Error::InvalidArgument};
      }
      adapters[adapter_index++] = impl->polymarket_adapter.get();
      account_routes[adapter_index - 1] = {
          config.polymarket.account_id, exchange::AdapterKind::Polymarket};
      drivers[driver_index++] = impl->polymarket_transport.get();
      drivers[driver_index++] = impl->polymarket_data_transport.get();
    }

    AdapterRuntimeConfig adapter_config{};
    adapter_config.adapters =
        std::span<TradeAdapter* const>(adapters.data(), adapter_index);
    adapter_config.enable_fake_fallback = false;
    adapter_config.event_budget = config.event_budget;
    adapter_config.io_drivers =
        std::span<AsyncIoDriver* const>(drivers.data(), driver_index);
    adapter_config.account_routes = std::span<const AdapterRuntimeConfig::AccountRoute>(
        account_routes.data(), adapter_index);
    auto created =
        OmsApi::Create(config.runtime, instruments, {}, adapter_config);
    if (!created) return {{}, created.error};
    impl->api = std::move(created.value);
    return {std::unique_ptr<ExecutionChannel>(
                new ExecutionChannel(std::move(impl))),
            Error::Ok};
  } catch (...) {
    return {{}, Error::NotReady};
  }
}

Result<void> ExecutionChannel::initialize_lane(
    std::uint32_t lane_id, std::uint32_t session_epoch) noexcept {
  return impl_->api->initialize_lane(lane_id, session_epoch);
}

Result<RequestToken> ExecutionChannel::place_order(
    std::uint32_t lane_id, SubmitOrderRequest request) noexcept {
  return impl_->api->submit_order_impl(lane_id, request);
}

Result<RequestToken> ExecutionChannel::cancel_order(
    std::uint32_t lane_id, RequestToken target, OrderHandle handle) noexcept {
  return impl_->api->cancel_order(lane_id, target, handle);
}

Result<RequestToken> ExecutionChannel::register_instrument(
    std::uint32_t lane_id, RegisterInstrumentRequest request) noexcept {
  return impl_->api->register_instrument(lane_id, request);
}

Result<RequestToken> ExecutionChannel::retire_instrument(
    std::uint32_t lane_id, InstrumentId instrument_id) noexcept {
  return impl_->api->retire_instrument(lane_id, instrument_id);
}

Result<QueryToken> ExecutionChannel::query_open_orders(
    std::uint32_t lane_id, AccountId account_id) noexcept {
  return impl_->api->query_open_orders(lane_id, account_id);
}

Result<QueryToken> ExecutionChannel::query_open_orders(
    std::uint32_t lane_id, QueryRequest request) noexcept {
  return impl_->api->query_open_orders(lane_id, request);
}

Result<QueryToken> ExecutionChannel::query_positions(
    std::uint32_t lane_id, AccountId account_id) noexcept {
  return impl_->api->query_positions(lane_id, account_id);
}

Result<QueryToken> ExecutionChannel::query_positions(
    std::uint32_t lane_id, QueryRequest request) noexcept {
  return impl_->api->query_positions(lane_id, request);
}

Error ExecutionChannel::service_io(int timeout_ms) noexcept {
  return impl_->api->service_io(timeout_ms);
}

std::size_t ExecutionChannel::drain_updates(
    std::uint32_t lane_id, UpdateCallback callback, void* context,
    std::size_t maximum) noexcept {
  return impl_->api->drain_updates(lane_id, callback, context, maximum);
}

int ExecutionChannel::notification_fd(std::uint32_t lane_id) const noexcept {
  return impl_->api->update_fd(lane_id);
}

RuntimeMetrics ExecutionChannel::metrics() const noexcept {
  return impl_->api->metrics();
}

Result<AdapterStatusSnapshot> ExecutionChannel::venue_status(
    exchange::AdapterKind kind) const noexcept {
  return impl_->api->adapter_status(kind);
}

Error ExecutionChannel::reconcile(exchange::AdapterKind kind) noexcept {
  return impl_->api->reconcile(kind);
}

Error ExecutionChannel::shutdown() noexcept { return impl_->api->shutdown(); }

}  // namespace oms::api
