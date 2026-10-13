cmake_minimum_required(VERSION 3.20)
project(selfquant_mds VERSION 0.1.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(MDS_BUILD_TESTS "Build MDS unit tests" ON)
option(MDS_BUILD_EXAMPLES "Build MDS examples" ON)
option(MDS_ENABLE_SIMDJSON "Enable simdjson OnDemand JSON parsing" ON)

find_package(yaml-cpp REQUIRED)
if(MDS_ENABLE_SIMDJSON)
  find_package(simdjson CONFIG QUIET)
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
  src/record/clickhouse_aggregate_recorder.cpp
  src/service/inprocess_aggregation.cpp
  src/service/binance_session.cpp
  src/service/market_data_session.cpp
  src/service/venue_connection.cpp
  src/service/mds_service.cpp
  src/transport/shared_ring.cpp
  src/exchange/capabilities.cpp
  src/exchange/venue_adapter.cpp
  src/exchange/aster/aster_adapter.cpp
  src/exchange/okx/okx_adapter.cpp
  src/exchange/bybit/bybit_adapter.cpp
  src/exchange/bitget/bitget_adapter.cpp
  src/exchange/gate/gate_adapter.cpp
  src/exchange/hyperliquid/hyperliquid_adapter.cpp
  src/exchange/lighter/lighter_adapter.cpp
  src/exchange/polymarket/polymarket_adapter.cpp
  src/exchange/binance/binance_adapter.cpp
  src/exchange/binance/binance_venue_adapter.cpp
  src/exchange/binance/binance_rest.cpp
  src/exchange/binance/binance_streams.cpp
  src/gateway/gateway.cpp
  src/instrument/instrument_manager.cpp
  src/redundancy/sequence_arbiter.cpp
  src/ibkr/ibkr_bbo_assembler.cpp
  src/ibkr/ibkr_client.cpp
  src/ibkr/ibkr_runtime.cpp
  src/ibkr/ibkr_universe.cpp
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

set(MDS_BUILD_VERSION_SOURCE
  "${CMAKE_CURRENT_BINARY_DIR}/generated/mds_build_version.cpp")
set_source_files_properties("${MDS_BUILD_VERSION_SOURCE}"
  PROPERTIES GENERATED TRUE)
add_custom_target(mds_build_version
  COMMAND "${CMAKE_COMMAND}"
    -DSOURCE_ROOT=${CMAKE_CURRENT_SOURCE_DIR}
    -DOUTPUT_FILE=${MDS_BUILD_VERSION_SOURCE}
    -P "${CMAKE_CURRENT_SOURCE_DIR}/generate_build_version.cmake"
  BYPRODUCTS "${MDS_BUILD_VERSION_SOURCE}"
  COMMENT "Generating MDS producer build identity"
  VERBATIM)

add_library(mds_producer_runtime STATIC
  cmd/mds_producer/producer_config.cpp
  src/producer/producer_runtime.cpp
  "${MDS_BUILD_VERSION_SOURCE}")
add_dependencies(mds_producer_runtime mds_build_version)
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
target_compile_options(mds_consumer_config PRIVATE -Wall -Wextra -Wpedantic)

add_executable(mds_shm_consumer
  examples/shm_consumer.cpp
  examples/aggregate_dashboard.cpp)
target_include_directories(mds_shm_consumer PRIVATE
  ${CMAKE_CURRENT_SOURCE_DIR}/examples)
target_link_libraries(mds_shm_consumer PRIVATE mds_consumer_config mds)
if(SELF_QUANT_INSTALL)
  install(TARGETS mds_shm_consumer RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
endif()

add_library(mds_ibkr_config STATIC
  cmd/mds_ibkr_producer/ibkr_candidates.cpp
  cmd/mds_ibkr_producer/ibkr_config.cpp)
target_include_directories(mds_ibkr_config
  PUBLIC
    ${CMAKE_CURRENT_SOURCE_DIR}/cmd/mds_ibkr_producer
    ${CMAKE_CURRENT_SOURCE_DIR}/include)
target_link_libraries(mds_ibkr_config PUBLIC mds)
find_package(PostgreSQL REQUIRED)
if(TARGET PostgreSQL::PostgreSQL)
  target_link_libraries(mds_ibkr_config PUBLIC PostgreSQL::PostgreSQL)
else()
  target_include_directories(mds_ibkr_config PUBLIC ${PostgreSQL_INCLUDE_DIRS})
  target_link_libraries(mds_ibkr_config PUBLIC ${PostgreSQL_LIBRARIES})
endif()
if(TARGET yaml-cpp::yaml-cpp)
  target_link_libraries(mds_ibkr_config PRIVATE yaml-cpp::yaml-cpp)
else()
  target_link_libraries(mds_ibkr_config PRIVATE yaml-cpp)
endif()
target_compile_options(mds_ibkr_config PRIVATE -Wall -Wextra -Wpedantic)
if(TARGET self_quant_warnings)
  target_link_libraries(mds_ibkr_config PRIVATE self_quant_warnings)
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
    MDS_PRODUCER_EXAMPLE_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/mds_producer.example.yaml"
    MDS_ASTER_LIGHTER_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/aster-lighter.yaml"
    MDS_ASTER_LIGHTER_BOOK_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/aster-lighter-book.yaml")
  add_test(NAME mds_producer_config_tests COMMAND mds_producer_config_tests)
  add_executable(mds_producer_runtime_tests
    tests/producer_runtime_tests.cpp)
  target_link_libraries(mds_producer_runtime_tests PRIVATE
    mds_producer_runtime)
  target_compile_definitions(mds_producer_runtime_tests PRIVATE
    MDS_PRODUCER_EXAMPLE_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/mds_producer.example.yaml")
  target_compile_options(mds_producer_runtime_tests PRIVATE
    $<$<CONFIG:Release>:-UNDEBUG>)
  add_test(NAME mds_producer_runtime_tests COMMAND mds_producer_runtime_tests)
  set_tests_properties(mds_producer_runtime_tests PROPERTIES TIMEOUT 900)
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
    mds_venue_connection_sharding_loopback_tests PROPERTIES TIMEOUT 120)
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
    MDS_CONSUMER_EXAMPLE_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/mds_shm_consumer.example.yaml"
    MDS_ASTER_LIGHTER_CONSUMER_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/aster-lighter-clickhouse.yaml")
  add_test(NAME mds_consumer_config_tests
    COMMAND mds_consumer_config_tests)
  add_executable(mds_shm_consumer_demux_tests
    tests/shm_consumer_demux_tests.cpp
    examples/aggregate_dashboard.cpp)
  target_include_directories(mds_shm_consumer_demux_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/examples)
  target_link_libraries(mds_shm_consumer_demux_tests PRIVATE
    mds_consumer_config mds)
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
  add_executable(mds_clickhouse_aggregate_recorder_tests
    tests/clickhouse_aggregate_recorder_tests.cpp)
  target_link_libraries(mds_clickhouse_aggregate_recorder_tests PRIVATE mds)
  target_compile_definitions(mds_clickhouse_aggregate_recorder_tests PRIVATE
    MDS_AGGREGATE_HISTORY_SQL="${CMAKE_CURRENT_SOURCE_DIR}/../../backend/deploy/clickhouse/migrations/002_aggregate_history.sql"
    MDS_CLICKHOUSE_AGGREGATE_RECORDER_H="${CMAKE_CURRENT_SOURCE_DIR}/include/mds/record/clickhouse_aggregate_recorder.h"
    MDS_CLICKHOUSE_AGGREGATE_RECORDER_CPP="${CMAKE_CURRENT_SOURCE_DIR}/src/record/clickhouse_aggregate_recorder.cpp")
  add_test(NAME mds_clickhouse_aggregate_recorder_tests
    COMMAND mds_clickhouse_aggregate_recorder_tests)
  set_tests_properties(mds_clickhouse_aggregate_recorder_tests PROPERTIES TIMEOUT 20)
  add_executable(mds_shm_consumer_sigpipe_tests
    tests/shm_consumer_sigpipe_tests.cpp)
  target_link_libraries(mds_shm_consumer_sigpipe_tests PRIVATE mds)
  target_compile_definitions(mds_shm_consumer_sigpipe_tests PRIVATE
    MDS_SHM_CONSUMER_BIN="$<TARGET_FILE:mds_shm_consumer>")
  add_dependencies(mds_shm_consumer_sigpipe_tests mds_shm_consumer)
  add_test(NAME mds_shm_consumer_sigpipe_tests
    COMMAND mds_shm_consumer_sigpipe_tests)
  set_tests_properties(mds_shm_consumer_sigpipe_tests PROPERTIES TIMEOUT 20)
  add_executable(mds_ibkr_bbo_assembler_tests
    tests/ibkr_bbo_assembler_tests.cpp)
  target_link_libraries(mds_ibkr_bbo_assembler_tests PRIVATE mds)
  add_test(NAME mds_ibkr_bbo_assembler_tests
    COMMAND mds_ibkr_bbo_assembler_tests)
  add_executable(mds_ibkr_config_tests tests/ibkr_config_tests.cpp)
  target_link_libraries(mds_ibkr_config_tests PRIVATE mds_ibkr_config)
  target_compile_definitions(mds_ibkr_config_tests PRIVATE
    MDS_IBKR_EXAMPLE_CONFIG="${CMAKE_CURRENT_SOURCE_DIR}/config/mds_ibkr.example.yaml")
  add_test(NAME mds_ibkr_config_tests COMMAND mds_ibkr_config_tests)
  add_executable(mds_ibkr_runtime_tests tests/ibkr_runtime_tests.cpp)
  target_link_libraries(mds_ibkr_runtime_tests PRIVATE mds)
  add_test(NAME mds_ibkr_runtime_tests COMMAND mds_ibkr_runtime_tests)
  set_tests_properties(mds_ibkr_runtime_tests PROPERTIES TIMEOUT 60)
  add_executable(mds_ibkr_universe_tests tests/ibkr_universe_tests.cpp)
  target_link_libraries(mds_ibkr_universe_tests PRIVATE mds)
  add_test(NAME mds_ibkr_universe_tests COMMAND mds_ibkr_universe_tests)
  add_test(NAME mds_ibkr_restart_ring_cleanup
    COMMAND bash ${CMAKE_CURRENT_SOURCE_DIR}/tests/restart_recoder_ibkr_ring_test.sh)
  add_test(NAME mds_ibkr_clickhouse_validate_example
    COMMAND mds_shm_consumer
      --config ${CMAKE_CURRENT_SOURCE_DIR}/config/mds_ibkr_clickhouse.example.yaml
      --clickhouse-bbo --validate-only)
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

option(SELF_QUANT_ENABLE_IBKR_MDS
  "Build mds_ibkr_producer against the official TWS API Stable 10.45" OFF)
set(IBKR_DEPS_ROOT "${CMAKE_SOURCE_DIR}/.deps/ibkr" CACHE PATH
  "Root of bootstrapped IBKR build dependencies")
set(IBKR_TWS_API_ROOT
  "${IBKR_DEPS_ROOT}/src/twsapi-10.45.01/IBJts" CACHE PATH
  "Root of the official TWS API Stable 10.45 C++ client")
set(IBKR_TWS_API_VERSION "10.45" CACHE STRING
  "Required official TWS API version")
set(IBKR_DECIMAL_LIBRARY
  "${IBKR_DEPS_ROOT}/prefix/lib/libbid.a" CACHE FILEPATH
  "Intel decimal floating-point library required by TWS Decimal.cpp")

if(SELF_QUANT_ENABLE_IBKR_MDS)
  if(NOT IBKR_TWS_API_VERSION STREQUAL "10.45")
    message(FATAL_ERROR
      "IBKR MDS requires the official TWS API Stable 10.45, not ${IBKR_TWS_API_VERSION}")
  endif()
  if(NOT IBKR_TWS_API_ROOT)
    message(FATAL_ERROR
      "SELF_QUANT_ENABLE_IBKR_MDS=ON requires IBKR_TWS_API_ROOT")
  endif()
  if(NOT EXISTS "${IBKR_TWS_API_ROOT}/API_VersionNum.txt")
    message(FATAL_ERROR
      "TWS API version file is missing under ${IBKR_TWS_API_ROOT}; "
      "run scripts/bootstrap-ibkr-deps.sh --accept-ib-license")
  endif()
  file(READ "${IBKR_TWS_API_ROOT}/API_VersionNum.txt" IBKR_API_VERSION_FILE)
  if(NOT IBKR_API_VERSION_FILE MATCHES "API_Version=10\\.45(\\.|$)")
    message(FATAL_ERROR
      "IBKR MDS requires TWS API 10.45; found ${IBKR_API_VERSION_FILE}")
  endif()
  find_path(IBKR_TWS_CLIENT_DIR EClientSocket.h
    PATHS
      "${IBKR_TWS_API_ROOT}"
      "${IBKR_TWS_API_ROOT}/client"
      "${IBKR_TWS_API_ROOT}/source/cppclient/client"
    NO_DEFAULT_PATH)
  if(NOT IBKR_TWS_CLIENT_DIR)
    message(FATAL_ERROR
      "EClientSocket.h was not found under IBKR_TWS_API_ROOT=${IBKR_TWS_API_ROOT}")
  endif()
  file(GLOB IBKR_TWS_API_SOURCES CONFIGURE_DEPENDS
    "${IBKR_TWS_CLIENT_DIR}/*.cpp")
  list(FILTER IBKR_TWS_API_SOURCES EXCLUDE REGEX
    ".*/(Main|TestCppClient|AvailableAlgoParams)\\.cpp$")
  list(FILTER IBKR_TWS_API_SOURCES EXCLUDE REGEX
    ".*\\.pb\\.(cpp|cc)$")
  if(NOT IBKR_TWS_API_SOURCES)
    message(FATAL_ERROR
      "No TWS API client sources found in ${IBKR_TWS_CLIENT_DIR}")
  endif()
  find_package(Protobuf QUIET)
  if(NOT Protobuf_FOUND)
    message(FATAL_ERROR
      "IBKR MDS requires protobuf for the official TWS API")
  endif()
  set(IBKR_TWS_PROTO_DIR "${IBKR_TWS_API_ROOT}/source/proto")
  file(GLOB IBKR_TWS_PROTO_FILES CONFIGURE_DEPENDS
    "${IBKR_TWS_PROTO_DIR}/*.proto")
  if(NOT IBKR_TWS_PROTO_FILES)
    message(FATAL_ERROR
      "No TWS protobuf definitions found under ${IBKR_TWS_PROTO_DIR}")
  endif()
  set(IBKR_TWS_PROTO_OUTPUT_DIR
    "${CMAKE_CURRENT_BINARY_DIR}/generated/ibkr-proto")
  set(IBKR_TWS_PROTO_SOURCES)
  set(IBKR_TWS_PROTO_HEADERS)
  foreach(IBKR_TWS_PROTO_FILE IN LISTS IBKR_TWS_PROTO_FILES)
    get_filename_component(IBKR_TWS_PROTO_NAME
      "${IBKR_TWS_PROTO_FILE}" NAME_WE)
    list(APPEND IBKR_TWS_PROTO_SOURCES
      "${IBKR_TWS_PROTO_OUTPUT_DIR}/${IBKR_TWS_PROTO_NAME}.pb.cc")
    list(APPEND IBKR_TWS_PROTO_HEADERS
      "${IBKR_TWS_PROTO_OUTPUT_DIR}/${IBKR_TWS_PROTO_NAME}.pb.h")
  endforeach()
  add_custom_command(
    OUTPUT ${IBKR_TWS_PROTO_SOURCES} ${IBKR_TWS_PROTO_HEADERS}
    COMMAND "${CMAKE_COMMAND}" -E make_directory
      "${IBKR_TWS_PROTO_OUTPUT_DIR}"
    COMMAND "${Protobuf_PROTOC_EXECUTABLE}"
      --experimental_allow_proto3_optional
      "--proto_path=${IBKR_TWS_PROTO_DIR}"
      "--cpp_out=${IBKR_TWS_PROTO_OUTPUT_DIR}"
      ${IBKR_TWS_PROTO_FILES}
    DEPENDS ${IBKR_TWS_PROTO_FILES}
    COMMENT "Generating TWS 10.45 protobuf C++ sources"
    VERBATIM)

  if(NOT EXISTS "${IBKR_DECIMAL_LIBRARY}")
    message(FATAL_ERROR
      "IBKR decimal library is missing at ${IBKR_DECIMAL_LIBRARY}; "
      "run scripts/bootstrap-ibkr-deps.sh --accept-ib-license")
  endif()
  if(NOT CMAKE_NM)
    find_program(CMAKE_NM NAMES llvm-nm nm REQUIRED)
  endif()
  execute_process(
    COMMAND "${CMAKE_NM}" -g --defined-only "${IBKR_DECIMAL_LIBRARY}"
    RESULT_VARIABLE IBKR_DECIMAL_NM_RESULT
    OUTPUT_VARIABLE IBKR_DECIMAL_SYMBOLS
    ERROR_VARIABLE IBKR_DECIMAL_NM_ERROR)
  if(NOT IBKR_DECIMAL_NM_RESULT EQUAL 0 OR
     NOT IBKR_DECIMAL_SYMBOLS MATCHES "__bid64_to_string" OR
     NOT IBKR_DECIMAL_SYMBOLS MATCHES "__bid64_from_string")
    message(FATAL_ERROR
      "${IBKR_DECIMAL_LIBRARY} is not a compatible Intel libbid "
      "(missing __bid64_to_string/__bid64_from_string): "
      "${IBKR_DECIMAL_NM_ERROR}")
  endif()

  add_library(ibkr_tws_api STATIC
    ${IBKR_TWS_API_SOURCES}
    ${IBKR_TWS_PROTO_SOURCES})
  target_include_directories(ibkr_tws_api PUBLIC
    "${IBKR_TWS_CLIENT_DIR}"
    "${IBKR_TWS_PROTO_OUTPUT_DIR}")
  if(TARGET protobuf::libprotobuf)
    target_link_libraries(ibkr_tws_api PUBLIC protobuf::libprotobuf)
  else()
    target_link_libraries(ibkr_tws_api PUBLIC ${Protobuf_LIBRARIES})
    target_include_directories(ibkr_tws_api PUBLIC ${Protobuf_INCLUDE_DIRS})
  endif()
  target_link_libraries(ibkr_tws_api PUBLIC "${IBKR_DECIMAL_LIBRARY}")
  target_compile_options(ibkr_tws_api PRIVATE -w)

  add_executable(mds_ibkr_producer
    cmd/mds_ibkr_producer/main.cpp
    src/ibkr/ibkr_tws_client.cpp)
  target_compile_definitions(mds_ibkr_producer PRIVATE MDS_HAS_IBKR=1)
  target_link_libraries(mds_ibkr_producer PRIVATE
    mds_ibkr_config mds ibkr_tws_api)
  target_compile_options(mds_ibkr_producer PRIVATE -Wall -Wextra -Wpedantic)
  if(SELF_QUANT_INSTALL)
    install(TARGETS mds_ibkr_producer RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
  endif()
else()
  message(STATUS
    "mds_ibkr_producer is disabled; configure -DSELF_QUANT_ENABLE_IBKR_MDS=ON "
    "after running scripts/bootstrap-ibkr-deps.sh --accept-ib-license")
endif()
