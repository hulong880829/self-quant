cmake_minimum_required(VERSION 3.20)
project(selfquant_mds VERSION 0.1.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(MDS_BUILD_TESTS "Build MDS unit tests" ON)
option(MDS_BUILD_EXAMPLES "Build MDS examples" ON)
option(MDS_ENABLE_SIMDJSON "Enable simdjson OnDemand JSON parsing" ON)
option(MDS_ENABLE_RECORDING "Enable zstd-backed consumer recording" ON)

find_package(yaml-cpp REQUIRED)
if(MDS_ENABLE_SIMDJSON)
  find_package(simdjson CONFIG QUIET)
endif()
set(MDS_RECORDING_AVAILABLE OFF)
if(MDS_ENABLE_RECORDING)
  find_package(PkgConfig QUIET)
  if(PkgConfig_FOUND)
    pkg_check_modules(ZSTD QUIET IMPORTED_TARGET libzstd)
  endif()
  if(TARGET PkgConfig::ZSTD)
    add_library(mds_recording_zstd INTERFACE)
    target_link_libraries(mds_recording_zstd INTERFACE PkgConfig::ZSTD)
    set(MDS_RECORDING_AVAILABLE ON)
  else()
    find_path(MDS_ZSTD_INCLUDE_DIR zstd.h)
    find_library(MDS_ZSTD_LIBRARY NAMES zstd)
    if(MDS_ZSTD_INCLUDE_DIR AND MDS_ZSTD_LIBRARY)
      add_library(mds_recording_zstd INTERFACE)
      target_include_directories(mds_recording_zstd INTERFACE
        ${MDS_ZSTD_INCLUDE_DIR})
      target_link_libraries(mds_recording_zstd INTERFACE
        ${MDS_ZSTD_LIBRARY})
      set(MDS_RECORDING_AVAILABLE ON)
    endif()
  endif()
  if(MDS_RECORDING_AVAILABLE)
    set(SELF_QUANT_HAS_RECORDING ON PARENT_SCOPE)
    message(STATUS "MDS recording support enabled with libzstd")
  else()
    set(SELF_QUANT_HAS_RECORDING OFF PARENT_SCOPE)
    message(STATUS
      "libzstd not found: recording is unavailable; producer, aggregator, "
      "libmds, print consumer, and gateway remain buildable")
  endif()
else()
  set(SELF_QUANT_HAS_RECORDING OFF PARENT_SCOPE)
  message(STATUS
    "MDS recording disabled: print consumer and gateway remain buildable")
endif()

add_library(mds STATIC
  src/agg/aggregation_engine.cpp
  src/agg/venue_ingest.cpp
  src/book/bbo_overlay.cpp
  src/book/book_bridge.cpp
  src/book/book_pipeline.cpp
  src/consume/aggregate_dispatch.cpp
  src/consume/aggregate_segment.cpp
  src/consume/order_book_reconstructor.cpp
  src/consume/segment_reader.cpp
  src/consume/sequence_tracker.cpp
  src/publish/wire_publisher.cpp
  src/record/clickhouse_bbo_recorder.cpp
  src/service/inprocess_aggregation.cpp
  src/service/binance_session.cpp
  src/service/market_data_session.cpp
  src/service/venue_connection.cpp
  src/service/mds_service.cpp
  src/transport/shared_ring.cpp
  src/exchange/capabilities.cpp
  src/exchange/venue_adapter.cpp
  src/exchange/okx/okx_adapter.cpp
  src/exchange/bybit/bybit_adapter.cpp
  src/exchange/bitget/bitget_adapter.cpp
  src/exchange/gate/gate_adapter.cpp
  src/exchange/hyperliquid/hyperliquid_adapter.cpp
  src/exchange/polymarket/polymarket_adapter.cpp
  src/exchange/binance/binance_adapter.cpp
  src/exchange/binance/binance_venue_adapter.cpp
  src/exchange/binance/binance_rest.cpp
  src/exchange/binance/binance_streams.cpp
  src/gateway/gateway.cpp
  src/instrument/instrument_manager.cpp
  src/redundancy/sequence_arbiter.cpp
)
add_library(self_quant::mds ALIAS mds)
set_target_properties(mds PROPERTIES EXPORT_NAME mds)
target_include_directories(mds
  PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>
  PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal
)
target_link_libraries(mds
  PUBLIC
    self_quant::utils
    self_quant::net)
target_compile_options(mds PRIVATE -Wall -Wextra -Wpedantic)
if(TARGET self_quant_warnings)
  target_link_libraries(mds PRIVATE self_quant_warnings)
endif()

if(TARGET simdjson::simdjson)
  set(SELF_QUANT_HAS_SIMDJSON ON PARENT_SCOPE)
  target_link_libraries(mds PUBLIC simdjson::simdjson)
  target_compile_definitions(mds PUBLIC MDS_HAS_SIMDJSON=1)
else()
  set(SELF_QUANT_HAS_SIMDJSON OFF PARENT_SCOPE)
  message(STATUS
    "simdjson not found: Binance JSON parser returns a clear runtime error; "
    "install simdjson or configure with -DMDS_ENABLE_SIMDJSON=OFF")
endif()

if(MDS_RECORDING_AVAILABLE)
  add_library(mds_record STATIC
    src/record/record_format.cpp
    src/record/record_reader.cpp
    src/record/recorder.cpp)
  add_library(self_quant::mds_record ALIAS mds_record)
  set_target_properties(mds_record PROPERTIES EXPORT_NAME mds_record)
  target_include_directories(mds_record PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_record
    PUBLIC mds
    PRIVATE mds_recording_zstd)
  target_compile_definitions(mds_record PUBLIC MDS_HAS_ZSTD=1)
  target_compile_options(mds_record PRIVATE -Wall -Wextra -Wpedantic)
endif()

add_library(mds_producer_runtime STATIC
  cmd/mds_producer/producer_config.cpp
  src/producer/producer_runtime.cpp)
add_library(self_quant::mds_producer_runtime ALIAS mds_producer_runtime)
set_target_properties(mds_producer_runtime PROPERTIES
  EXPORT_NAME mds_producer_runtime)
target_include_directories(mds_producer_runtime
  PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>
  PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/cmd/mds_producer
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
target_link_libraries(mds_producer_runtime
  PUBLIC mds)
if(TARGET yaml-cpp::yaml-cpp)
  target_link_libraries(mds_producer_runtime PRIVATE yaml-cpp::yaml-cpp)
else()
  target_link_libraries(mds_producer_runtime PRIVATE yaml-cpp)
endif()
target_compile_options(mds_producer_runtime PRIVATE
  -Wall -Wextra -Wpedantic)
if(TARGET self_quant_warnings)
  target_link_libraries(mds_producer_runtime PRIVATE self_quant_warnings)
endif()

add_executable(mds_producer cmd/mds_producer/main.cpp)
target_link_libraries(mds_producer PRIVATE mds_producer_runtime)
if(SELF_QUANT_INSTALL)
  install(TARGETS mds_producer RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
endif()

if(MDS_RECORDING_AVAILABLE)
  add_executable(mds_record_tool cmd/mds_record_tool/main.cpp)
  target_link_libraries(mds_record_tool PRIVATE mds_record)
  if(SELF_QUANT_INSTALL)
    install(TARGETS mds_record_tool RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
  endif()
endif()

add_library(mds_aggregator_config STATIC
  cmd/mds_aggregator/aggregator_config.cpp)
target_include_directories(mds_aggregator_config
  PUBLIC
    ${CMAKE_CURRENT_SOURCE_DIR}/cmd/mds_aggregator
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
target_link_libraries(mds_aggregator_config PUBLIC mds)
if(TARGET yaml-cpp::yaml-cpp)
  target_link_libraries(mds_aggregator_config PRIVATE yaml-cpp::yaml-cpp)
else()
  target_link_libraries(mds_aggregator_config PRIVATE yaml-cpp)
endif()
target_compile_options(mds_aggregator_config PRIVATE -Wall -Wextra -Wpedantic)

add_executable(mds_aggregator cmd/mds_aggregator/main.cpp)
target_include_directories(mds_aggregator PRIVATE
  ${CMAKE_CURRENT_SOURCE_DIR}/internal)
target_link_libraries(mds_aggregator PRIVATE mds_aggregator_config mds)
if(SELF_QUANT_INSTALL)
  install(TARGETS mds_aggregator RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
endif()

add_library(mds_consumer_config STATIC
  cmd/mds_shm_consumer/consumer_config.cpp)
target_include_directories(mds_consumer_config
  PUBLIC
    ${CMAKE_CURRENT_SOURCE_DIR}/cmd/mds_shm_consumer
  PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
target_link_libraries(mds_consumer_config PUBLIC mds)
if(TARGET yaml-cpp::yaml-cpp)
  target_link_libraries(mds_consumer_config PRIVATE yaml-cpp::yaml-cpp)
else()
  target_link_libraries(mds_consumer_config PRIVATE yaml-cpp)
endif()
if(MDS_RECORDING_AVAILABLE)
  target_compile_definitions(mds_consumer_config PRIVATE MDS_HAS_ZSTD=1)
endif()
target_compile_options(mds_consumer_config PRIVATE -Wall -Wextra -Wpedantic)

add_executable(mds_shm_consumer
  examples/shm_consumer.cpp
  examples/aggregate_dashboard.cpp)
target_include_directories(mds_shm_consumer PRIVATE
  ${CMAKE_CURRENT_SOURCE_DIR}/examples)
target_link_libraries(mds_shm_consumer PRIVATE mds_consumer_config mds)
if(MDS_RECORDING_AVAILABLE)
  target_link_libraries(mds_shm_consumer PRIVATE mds_record)
endif()
if(SELF_QUANT_INSTALL)
  install(TARGETS mds_shm_consumer RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
endif()

if(MDS_BUILD_TESTS)
  enable_testing()
  add_executable(mds_tests tests/mds_tests.cpp)
  target_link_libraries(mds_tests PRIVATE mds)
  target_include_directories(mds_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_compile_definitions(mds_tests PRIVATE
    MDS_TEST_FIXTURE_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/fixtures")
  add_test(NAME mds_tests COMMAND mds_tests)
  add_executable(mds_network_tests tests/network_tests.cpp)
  target_link_libraries(mds_network_tests PRIVATE mds)
  target_include_directories(mds_network_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  add_test(NAME mds_network_tests COMMAND mds_network_tests)
  add_executable(mds_websocket_server_tests
    tests/websocket_server_tests.cpp)
  target_link_libraries(mds_websocket_server_tests PRIVATE mds)
  target_include_directories(mds_websocket_server_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  add_test(NAME mds_websocket_server_tests
    COMMAND mds_websocket_server_tests)
  set_tests_properties(mds_websocket_server_tests PROPERTIES TIMEOUT 10)
  add_executable(mds_gateway_tests tests/gateway_tests.cpp)
  target_link_libraries(mds_gateway_tests PRIVATE mds)
  target_include_directories(mds_gateway_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  add_test(NAME mds_gateway_tests COMMAND mds_gateway_tests)
  set_tests_properties(mds_gateway_tests PROPERTIES TIMEOUT 10)
  add_executable(mds_wire_publisher_tests tests/wire_publisher_tests.cpp)
  target_link_libraries(mds_wire_publisher_tests PRIVATE mds)
  add_test(NAME mds_wire_publisher_tests COMMAND mds_wire_publisher_tests)
  add_executable(mds_instrument_manager_tests
    tests/instrument_manager_tests.cpp)
  target_link_libraries(mds_instrument_manager_tests PRIVATE mds)
  add_test(NAME mds_instrument_manager_tests
    COMMAND mds_instrument_manager_tests)
  add_executable(mds_binance_loopback_integration_tests
    tests/binance_loopback_integration_tests.cpp)
  target_link_libraries(mds_binance_loopback_integration_tests PRIVATE mds)
  target_include_directories(mds_binance_loopback_integration_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_compile_definitions(mds_binance_loopback_integration_tests PRIVATE
    MDS_TEST_FIXTURE_DIR="${CMAKE_CURRENT_SOURCE_DIR}/tests/fixtures")
  add_test(NAME mds_binance_loopback_integration_tests
    COMMAND mds_binance_loopback_integration_tests)
  set_tests_properties(mds_binance_loopback_integration_tests PROPERTIES
    TIMEOUT 10)
  add_executable(mds_producer_config_tests tests/producer_config_tests.cpp)
  target_include_directories(mds_producer_config_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/cmd/mds_producer
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_producer_config_tests PRIVATE
    mds_producer_runtime)
  target_compile_definitions(mds_producer_config_tests PRIVATE
    MDS_PRODUCER_EXAMPLE_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/mds_producer.example.yaml")
  add_test(NAME mds_producer_config_tests COMMAND mds_producer_config_tests)
  add_executable(mds_producer_runtime_tests
    tests/producer_runtime_tests.cpp)
  target_link_libraries(mds_producer_runtime_tests PRIVATE
    mds_producer_runtime)
  target_compile_definitions(mds_producer_runtime_tests PRIVATE
    MDS_PRODUCER_EXAMPLE_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/mds_producer.example.yaml")
  add_test(NAME mds_producer_runtime_tests COMMAND mds_producer_runtime_tests)
  add_test(NAME mds_producer_validate_example
    COMMAND mds_producer
      --config ${CMAKE_CURRENT_SOURCE_DIR}/config/mds_producer.example.yaml
      --validate-only)
  add_test(NAME mds_polymarket_validate_example
    COMMAND mds_producer
      --config ${CMAKE_CURRENT_SOURCE_DIR}/config/mds_polymarket.example.yaml
      --validate-only)
  add_executable(mds_consumer_sequence_tests tests/consumer_sequence_tests.cpp)
  target_link_libraries(mds_consumer_sequence_tests PRIVATE mds)
  add_test(NAME mds_consumer_sequence_tests
    COMMAND mds_consumer_sequence_tests)
  add_executable(mds_aggregate_dispatch_tests
    tests/aggregate_dispatch_tests.cpp)
  target_link_libraries(mds_aggregate_dispatch_tests PRIVATE mds)
  add_test(NAME mds_aggregate_dispatch_tests
    COMMAND mds_aggregate_dispatch_tests)
  add_executable(mds_connection_deadlines_tests
    tests/connection_deadlines_tests.cpp)
  target_include_directories(mds_connection_deadlines_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_connection_deadlines_tests PRIVATE mds)
  add_test(NAME mds_connection_deadlines_tests
    COMMAND mds_connection_deadlines_tests)
  add_executable(mds_snapshot_http_policy_tests
    tests/snapshot_http_policy_tests.cpp)
  target_include_directories(mds_snapshot_http_policy_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_snapshot_http_policy_tests PRIVATE mds)
  add_test(NAME mds_snapshot_http_policy_tests
    COMMAND mds_snapshot_http_policy_tests)
  add_executable(mds_consumer_toolkit_tests tests/consumer_toolkit_tests.cpp)
  target_link_libraries(mds_consumer_toolkit_tests PRIVATE mds)
  add_test(NAME mds_consumer_toolkit_tests
    COMMAND mds_consumer_toolkit_tests)
  add_executable(mds_venue_capabilities_tests
    tests/venue_capabilities_tests.cpp)
  target_include_directories(mds_venue_capabilities_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_venue_capabilities_tests PRIVATE mds)
  add_test(NAME mds_venue_capabilities_tests
    COMMAND mds_venue_capabilities_tests)
  add_executable(mds_venue_adapter_tests
    tests/venue_adapter_tests.cpp)
  target_include_directories(mds_venue_adapter_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_venue_adapter_tests PRIVATE mds)
  add_test(NAME mds_venue_adapter_tests
    COMMAND mds_venue_adapter_tests)
  add_executable(mds_venue_connection_sharding_tests
    tests/venue_connection_sharding_tests.cpp)
  target_include_directories(mds_venue_connection_sharding_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_venue_connection_sharding_tests PRIVATE mds)
  add_test(NAME mds_venue_connection_sharding_tests
    COMMAND mds_venue_connection_sharding_tests)
  add_executable(mds_venue_connection_sharding_loopback_tests
    tests/venue_connection_sharding_loopback_tests.cpp)
  target_include_directories(
    mds_venue_connection_sharding_loopback_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(
    mds_venue_connection_sharding_loopback_tests PRIVATE mds)
  target_compile_options(
    mds_venue_connection_sharding_loopback_tests PRIVATE
    $<$<CONFIG:Release>:-UNDEBUG>)
  add_test(NAME mds_venue_connection_sharding_loopback_tests
    COMMAND mds_venue_connection_sharding_loopback_tests)
  set_tests_properties(
    mds_venue_connection_sharding_loopback_tests PROPERTIES TIMEOUT 30)
  add_executable(mds_polymarket_protocol_tests
    tests/polymarket_protocol_tests.cpp)
  target_include_directories(mds_polymarket_protocol_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_polymarket_protocol_tests PRIVATE mds)
  add_test(NAME mds_polymarket_protocol_tests
    COMMAND mds_polymarket_protocol_tests)
  add_executable(mds_aggregation_engine_tests
    tests/aggregation_engine_tests.cpp)
  target_include_directories(mds_aggregation_engine_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_aggregation_engine_tests PRIVATE mds)
  add_test(NAME mds_aggregation_engine_tests
    COMMAND mds_aggregation_engine_tests)
  add_executable(mds_inprocess_aggregation_tests
    tests/inprocess_aggregation_tests.cpp)
  target_include_directories(mds_inprocess_aggregation_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(mds_inprocess_aggregation_tests PRIVATE mds)
  add_test(NAME mds_inprocess_aggregation_tests
    COMMAND mds_inprocess_aggregation_tests)
  add_executable(mds_aggregator_config_tests
    tests/aggregator_config_tests.cpp)
  target_link_libraries(mds_aggregator_config_tests PRIVATE
    mds_aggregator_config)
  target_compile_definitions(mds_aggregator_config_tests PRIVATE
    MDS_AGGREGATOR_EXAMPLE_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/mds_aggregator.example.yaml")
  add_test(NAME mds_aggregator_config_tests
    COMMAND mds_aggregator_config_tests)
  add_test(NAME mds_aggregator_validate_example
    COMMAND mds_aggregator
      --config ${CMAKE_CURRENT_SOURCE_DIR}/config/mds_aggregator.example.yaml
      --validate-only)
  add_executable(mds_consumer_config_tests
    tests/consumer_config_tests.cpp)
  target_link_libraries(mds_consumer_config_tests PRIVATE
    mds_consumer_config)
  target_compile_definitions(mds_consumer_config_tests PRIVATE
    MDS_CONSUMER_EXAMPLE_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/mds_shm_consumer.example.yaml")
  add_test(NAME mds_consumer_config_tests
    COMMAND mds_consumer_config_tests)
  add_executable(mds_shm_consumer_demux_tests
    tests/shm_consumer_demux_tests.cpp
    examples/aggregate_dashboard.cpp)
  target_include_directories(mds_shm_consumer_demux_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/examples)
  target_link_libraries(mds_shm_consumer_demux_tests PRIVATE
    mds_consumer_config mds)
  if(MDS_RECORDING_AVAILABLE)
    target_link_libraries(mds_shm_consumer_demux_tests PRIVATE mds_record)
  endif()
  add_test(NAME mds_shm_consumer_demux_tests
    COMMAND mds_shm_consumer_demux_tests)
  add_test(NAME mds_consumer_validate_example
    COMMAND mds_shm_consumer
      --config ${CMAKE_CURRENT_SOURCE_DIR}/config/mds_shm_consumer.example.yaml
      --validate-only)
  add_test(NAME mds_clickhouse_bbo_validate_example
    COMMAND mds_shm_consumer
      --config ${CMAKE_CURRENT_SOURCE_DIR}/config/mds_clickhouse_bbo.example.yaml
      --clickhouse-bbo --validate-only)
  add_executable(mds_clickhouse_bbo_recorder_tests
    tests/clickhouse_bbo_recorder_tests.cpp)
  target_link_libraries(mds_clickhouse_bbo_recorder_tests PRIVATE mds)
  add_test(NAME mds_clickhouse_bbo_recorder_tests
    COMMAND mds_clickhouse_bbo_recorder_tests)
  set_tests_properties(mds_clickhouse_bbo_recorder_tests PROPERTIES TIMEOUT 10)
  if(MDS_RECORDING_AVAILABLE)
    add_executable(mds_recording_tests tests/recording_tests.cpp)
    target_link_libraries(mds_recording_tests PRIVATE mds_record)
    target_include_directories(mds_recording_tests PRIVATE
      ${CMAKE_CURRENT_SOURCE_DIR}/internal)
    add_test(NAME mds_recording_tests COMMAND mds_recording_tests)
    set_tests_properties(mds_recording_tests PROPERTIES TIMEOUT 10)
  endif()
endif()

if(MDS_BUILD_EXAMPLES)
  add_executable(binance_mds_demo examples/binance_mds_demo.cpp)
  target_link_libraries(binance_mds_demo PRIVATE mds)
  target_include_directories(binance_mds_demo PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  if(MDS_BUILD_TESTS)
    add_executable(mds_aggregate_dashboard_tests
      tests/aggregate_dashboard_tests.cpp
      examples/aggregate_dashboard.cpp)
    target_include_directories(mds_aggregate_dashboard_tests PRIVATE
      ${CMAKE_CURRENT_SOURCE_DIR}/examples)
    target_link_libraries(mds_aggregate_dashboard_tests PRIVATE mds)
    add_test(NAME mds_aggregate_dashboard_tests
      COMMAND mds_aggregate_dashboard_tests)
  endif()
  add_executable(equity_ticker_publisher examples/equity_ticker_publisher.cpp)
  target_link_libraries(equity_ticker_publisher PRIVATE mds)
  add_executable(equity_strategy_demo examples/equity_strategy_demo.cpp)
  target_link_libraries(equity_strategy_demo PRIVATE mds)
  if(MDS_BUILD_TESTS)
    add_test(NAME mds_equity_demo_integration_tests
      COMMAND bash
        ${CMAKE_CURRENT_SOURCE_DIR}/tests/equity_demo_integration.sh
        $<TARGET_FILE:equity_ticker_publisher>
        $<TARGET_FILE:equity_strategy_demo>)
    set_tests_properties(mds_equity_demo_integration_tests PROPERTIES
      TIMEOUT 8)
  endif()
endif()
