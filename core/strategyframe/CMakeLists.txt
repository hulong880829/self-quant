cmake_minimum_required(VERSION 3.20)
project(self_quant_strategyframe VERSION 0.1.0 LANGUAGES CXX)

option(STRATEGYFRAME_BUILD_TESTS "Build StrategyFrame tests" ON)
option(STRATEGYFRAME_BUILD_EXAMPLES "Build StrategyFrame examples" ON)
option(STRATEGYFRAME_ENABLE_LTO "Enable Release LTO for StrategyFrame" OFF)
option(STRATEGYFRAME_ENABLE_NATIVE_ARCH
  "Compile StrategyFrame for the build host CPU" OFF)
option(STRATEGYFRAME_PGO_GENERATE
  "Instrument StrategyFrame to generate GCC/Clang PGO data" OFF)
set(STRATEGYFRAME_PGO_PROFILE_DIR "" CACHE PATH
  "Directory containing GCC/Clang PGO data to consume")

find_package(yaml-cpp REQUIRED)

add_library(self_quant_strategyframe_config STATIC
  src/config.cpp)
add_library(self_quant::strategyframe_config ALIAS
  self_quant_strategyframe_config)
set_target_properties(self_quant_strategyframe_config PROPERTIES
  EXPORT_NAME strategyframe_config)
target_compile_features(self_quant_strategyframe_config PUBLIC cxx_std_20)
target_include_directories(self_quant_strategyframe_config
  PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>)
if(TARGET yaml-cpp::yaml-cpp)
  target_link_libraries(self_quant_strategyframe_config PRIVATE
    yaml-cpp::yaml-cpp)
else()
  target_link_libraries(self_quant_strategyframe_config PRIVATE yaml-cpp)
endif()

add_library(self_quant_strategyframe STATIC
  src/managers.cpp
  src/market_data_source.cpp
  src/runtime_core.cpp
  src/strategy_context.cpp)
add_library(self_quant::strategyframe ALIAS self_quant_strategyframe)
set_target_properties(self_quant_strategyframe PROPERTIES
  EXPORT_NAME strategyframe)
target_compile_features(self_quant_strategyframe PUBLIC cxx_std_20)
target_include_directories(self_quant_strategyframe
  PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:${CMAKE_INSTALL_INCLUDEDIR}>
  PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
target_link_libraries(self_quant_strategyframe
  PUBLIC self_quant_strategyframe_config
  PRIVATE self_quant::oms mds self_quant::mds_producer_runtime
    self_quant::net self_quant::utils)

if(STRATEGYFRAME_ENABLE_LTO)
  include(CheckIPOSupported)
  check_ipo_supported(RESULT strategyframe_ipo_supported
    OUTPUT strategyframe_ipo_error)
  if(NOT strategyframe_ipo_supported)
    message(FATAL_ERROR "StrategyFrame LTO unavailable: ${strategyframe_ipo_error}")
  endif()
  set_property(TARGET self_quant_strategyframe PROPERTY
    INTERPROCEDURAL_OPTIMIZATION_RELEASE TRUE)
endif()

if(STRATEGYFRAME_ENABLE_NATIVE_ARCH)
  target_compile_options(self_quant_strategyframe PRIVATE -march=native)
endif()

if(STRATEGYFRAME_PGO_GENERATE AND STRATEGYFRAME_PGO_PROFILE_DIR)
  message(FATAL_ERROR
    "Choose either STRATEGYFRAME_PGO_GENERATE or STRATEGYFRAME_PGO_PROFILE_DIR")
endif()
if(STRATEGYFRAME_PGO_GENERATE)
  target_compile_options(self_quant_strategyframe PRIVATE -fprofile-generate)
  target_link_options(self_quant_strategyframe INTERFACE -fprofile-generate)
elseif(STRATEGYFRAME_PGO_PROFILE_DIR)
  target_compile_options(self_quant_strategyframe PRIVATE
    "-fprofile-use=${STRATEGYFRAME_PGO_PROFILE_DIR}" -fprofile-correction)
  target_link_options(self_quant_strategyframe INTERFACE
    "-fprofile-use=${STRATEGYFRAME_PGO_PROFILE_DIR}" -fprofile-correction)
endif()

foreach(target self_quant_strategyframe self_quant_strategyframe_config)
  target_compile_options(${target} PRIVATE -Wall -Wextra -Wpedantic)
  if(TARGET self_quant_warnings)
    target_link_libraries(${target} PRIVATE self_quant_warnings)
  endif()
endforeach()

add_executable(strategyframe_config_tool cmd/config_tool.cpp)
target_link_libraries(strategyframe_config_tool PRIVATE
  self_quant_strategyframe_config)

add_executable(strategyframe_benchmark cmd/benchmark.cpp)
target_include_directories(strategyframe_benchmark PRIVATE
  ${CMAKE_CURRENT_SOURCE_DIR}/internal)
target_link_libraries(strategyframe_benchmark PRIVATE
  self_quant_strategyframe)
if(STRATEGYFRAME_ENABLE_LTO)
  set_property(TARGET strategyframe_benchmark PROPERTY
    INTERPROCEDURAL_OPTIMIZATION_RELEASE TRUE)
endif()
if(STRATEGYFRAME_ENABLE_NATIVE_ARCH)
  target_compile_options(strategyframe_benchmark PRIVATE -march=native)
endif()

if(STRATEGYFRAME_BUILD_EXAMPLES)
  add_executable(strategyframe_minimal_example examples/minimal_strategy.cpp)
  target_link_libraries(strategyframe_minimal_example PRIVATE
    self_quant_strategyframe)
  add_executable(strategyframe_arbitrage_example
    examples/cross_venue_arbitrage.cpp)
  target_link_libraries(strategyframe_arbitrage_example PRIVATE
    self_quant_strategyframe)
  add_executable(strategyframe_market_maker_example
    examples/market_maker_skeleton.cpp)
  target_link_libraries(strategyframe_market_maker_example PRIVATE
    self_quant_strategyframe)
endif()

if(STRATEGYFRAME_BUILD_TESTS)
  enable_testing()
  add_executable(strategyframe_contract_tests tests/contract_tests.cpp)
  target_link_libraries(strategyframe_contract_tests PRIVATE
    self_quant_strategyframe)
  add_test(NAME strategyframe_contract_tests
    COMMAND strategyframe_contract_tests)

  add_executable(strategyframe_manager_tests tests/manager_tests.cpp)
  target_include_directories(strategyframe_manager_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(strategyframe_manager_tests PRIVATE
    self_quant_strategyframe)
  add_test(NAME strategyframe_manager_tests
    COMMAND strategyframe_manager_tests)

  add_executable(strategyframe_hotpath_tests tests/hotpath_tests.cpp)
  target_include_directories(strategyframe_hotpath_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(strategyframe_hotpath_tests PRIVATE
    self_quant_strategyframe)
  add_test(NAME strategyframe_hotpath_tests
    COMMAND strategyframe_hotpath_tests)

  add_executable(strategyframe_runtime_tests tests/runtime_tests.cpp)
  target_link_libraries(strategyframe_runtime_tests PRIVATE
    self_quant_strategyframe)
  add_test(NAME strategyframe_runtime_tests
    COMMAND strategyframe_runtime_tests)

  add_executable(strategyframe_mds_tests
    tests/market_data_source_tests.cpp)
  target_include_directories(strategyframe_mds_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/internal)
  target_link_libraries(strategyframe_mds_tests PRIVATE
    self_quant_strategyframe mds)
  add_test(NAME strategyframe_mds_tests
    COMMAND strategyframe_mds_tests)

  add_executable(strategyframe_config_tests tests/config_tests.cpp)
  target_link_libraries(strategyframe_config_tests PRIVATE
    self_quant_strategyframe_config)
  target_compile_definitions(strategyframe_config_tests PRIVATE
    STRATEGYFRAME_CONFIG_FIXTURE="${CMAKE_CURRENT_SOURCE_DIR}/config/strategyframe.example.yaml")
  add_test(NAME strategyframe_config_tests
    COMMAND strategyframe_config_tests)

  add_test(NAME strategyframe_validate_example
    COMMAND strategyframe_config_tool
      --config ${CMAKE_CURRENT_SOURCE_DIR}/config/strategyframe.example.yaml
      --validate-only)
  add_test(NAME strategyframe_validate_self_hosted_example
    COMMAND strategyframe_config_tool
      --config ${CMAKE_CURRENT_SOURCE_DIR}/config/self_hosted.example.yaml
      --validate-only)
endif()
