cmake_minimum_required(VERSION 3.20)

project(self_quant_core
  VERSION 0.1.0
  DESCRIPTION "Low-latency market data bus core"
  LANGUAGES CXX)

include(CTest)
include(GNUInstallDirs)
include(CMakePackageConfigHelpers)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(SELF_QUANT_ENABLE_WERROR "Treat compiler warnings as errors" OFF)
option(SELF_QUANT_ENABLE_MDS "Build the market data service" ON)
option(SELF_QUANT_BUILD_BENCHMARKS "Build SDK hot-path benchmarks" OFF)
option(SELF_QUANT_BUILD_PACKAGE_TESTS "Build installed-package smoke tests" OFF)
option(SELF_QUANT_INSTALL "Install the self_quant SDK" ON)

add_library(self_quant_warnings INTERFACE)
set_target_properties(self_quant_warnings PROPERTIES EXPORT_NAME warnings)
target_compile_options(self_quant_warnings INTERFACE
  $<BUILD_INTERFACE:$<$<CXX_COMPILER_ID:GNU,Clang>:
    -Wall
    -Wextra
    -Wpedantic
    -Wconversion
    -Wsign-conversion
    $<$<BOOL:${SELF_QUANT_ENABLE_WERROR}>:-Werror>>>)

add_subdirectory(utils)

set(SELF_QUANT_HAS_SIMDJSON OFF)
if(SELF_QUANT_ENABLE_MDS)
  add_subdirectory(mds)
endif()

if(SELF_QUANT_BUILD_BENCHMARKS)
  if(NOT SELF_QUANT_ENABLE_MDS)
    message(FATAL_ERROR "SELF_QUANT_BUILD_BENCHMARKS requires SELF_QUANT_ENABLE_MDS")
  endif()
  add_executable(self_quant_sdk_hotpath_benchmark
    tests/perf/sdk_hotpath_benchmark.cpp)
  target_link_libraries(self_quant_sdk_hotpath_benchmark PRIVATE mds)
endif()

if(SELF_QUANT_BUILD_PACKAGE_TESTS)
  add_test(NAME self_quant_install_package_smoke
    COMMAND bash ${CMAKE_CURRENT_SOURCE_DIR}/tests/install_package_smoke.sh
      ${CMAKE_CURRENT_SOURCE_DIR}
      ${CMAKE_CURRENT_BINARY_DIR}/package-smoke)
endif()

if(SELF_QUANT_INSTALL)
  set(SELF_QUANT_EXPORT_TARGETS self_quant_warnings self_quant_utils)
  if(SELF_QUANT_ENABLE_MDS)
    list(APPEND SELF_QUANT_EXPORT_TARGETS mds)
  endif()

  install(TARGETS ${SELF_QUANT_EXPORT_TARGETS}
    EXPORT self_quantTargets
    ARCHIVE DESTINATION ${CMAKE_INSTALL_LIBDIR}
    INCLUDES DESTINATION ${CMAKE_INSTALL_INCLUDEDIR})
  install(DIRECTORY utils/include/ DESTINATION ${CMAKE_INSTALL_INCLUDEDIR})
  if(SELF_QUANT_ENABLE_MDS)
    install(DIRECTORY mds/include/ DESTINATION ${CMAKE_INSTALL_INCLUDEDIR})
  endif()
  install(EXPORT self_quantTargets
    NAMESPACE self_quant::
    FILE self_quant-targets.cmake
    DESTINATION ${CMAKE_INSTALL_LIBDIR}/cmake/self_quant)

  configure_package_config_file(
    cmake/self_quant-config.cmake.in
    ${CMAKE_CURRENT_BINARY_DIR}/self_quant-config.cmake
    INSTALL_DESTINATION ${CMAKE_INSTALL_LIBDIR}/cmake/self_quant)
  write_basic_package_version_file(
    ${CMAKE_CURRENT_BINARY_DIR}/self_quant-config-version.cmake
    VERSION ${PROJECT_VERSION}
    COMPATIBILITY SameMinorVersion)
  install(FILES
    ${CMAKE_CURRENT_BINARY_DIR}/self_quant-config.cmake
    ${CMAKE_CURRENT_BINARY_DIR}/self_quant-config-version.cmake
    DESTINATION ${CMAKE_INSTALL_LIBDIR}/cmake/self_quant)
endif()
