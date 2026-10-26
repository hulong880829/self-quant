function(self_quant_add_ibkr_tws_api)
  if(TARGET ibkr_tws_api)
    return()
  endif()
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
  if(NOT IBKR_TWS_API_VERSION STREQUAL "10.45")
    message(FATAL_ERROR
      "IBKR requires the official TWS API Stable 10.45, not ${IBKR_TWS_API_VERSION}")
  endif()
  if(NOT EXISTS "${IBKR_TWS_API_ROOT}/API_VersionNum.txt")
    message(FATAL_ERROR
      "TWS API version file is missing under ${IBKR_TWS_API_ROOT}; "
      "run scripts/bootstrap-ibkr-deps.sh --accept-ib-license")
  endif()
  file(READ "${IBKR_TWS_API_ROOT}/API_VersionNum.txt" IBKR_API_VERSION_FILE)
  if(NOT IBKR_API_VERSION_FILE MATCHES "API_Version=10\\.45(\\.|$)")
    message(FATAL_ERROR
      "IBKR requires TWS API 10.45; found ${IBKR_API_VERSION_FILE}")
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
  list(FILTER IBKR_TWS_API_SOURCES EXCLUDE REGEX ".*\\.pb\\.(cpp|cc)$")
  if(NOT IBKR_TWS_API_SOURCES)
    message(FATAL_ERROR "No TWS API client sources found in ${IBKR_TWS_CLIENT_DIR}")
  endif()
  find_package(Protobuf QUIET)
  if(NOT Protobuf_FOUND)
    message(FATAL_ERROR "IBKR requires protobuf for the official TWS API")
  endif()
  set(IBKR_TWS_PROTO_DIR "${IBKR_TWS_API_ROOT}/source/proto")
  file(GLOB IBKR_TWS_PROTO_FILES CONFIGURE_DEPENDS "${IBKR_TWS_PROTO_DIR}/*.proto")
  if(NOT IBKR_TWS_PROTO_FILES)
    message(FATAL_ERROR "No TWS protobuf definitions found under ${IBKR_TWS_PROTO_DIR}")
  endif()
  set(IBKR_TWS_PROTO_OUTPUT_DIR
    "${CMAKE_BINARY_DIR}/generated/ibkr-proto")
  set(IBKR_TWS_PROTO_SOURCES)
  set(IBKR_TWS_PROTO_HEADERS)
  foreach(IBKR_TWS_PROTO_FILE IN LISTS IBKR_TWS_PROTO_FILES)
    get_filename_component(IBKR_TWS_PROTO_NAME "${IBKR_TWS_PROTO_FILE}" NAME_WE)
    list(APPEND IBKR_TWS_PROTO_SOURCES
      "${IBKR_TWS_PROTO_OUTPUT_DIR}/${IBKR_TWS_PROTO_NAME}.pb.cc")
    list(APPEND IBKR_TWS_PROTO_HEADERS
      "${IBKR_TWS_PROTO_OUTPUT_DIR}/${IBKR_TWS_PROTO_NAME}.pb.h")
  endforeach()
  add_custom_command(
    OUTPUT ${IBKR_TWS_PROTO_SOURCES} ${IBKR_TWS_PROTO_HEADERS}
    COMMAND "${CMAKE_COMMAND}" -E make_directory "${IBKR_TWS_PROTO_OUTPUT_DIR}"
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
      "(missing __bid64_to_string/__bid64_from_string): ${IBKR_DECIMAL_NM_ERROR}")
  endif()
  add_library(ibkr_tws_api STATIC ${IBKR_TWS_API_SOURCES} ${IBKR_TWS_PROTO_SOURCES})
  set_target_properties(ibkr_tws_api PROPERTIES EXPORT_NAME ibkr_tws_api)
  add_library(self_quant::ibkr_tws_api ALIAS ibkr_tws_api)
  target_include_directories(ibkr_tws_api PUBLIC
    $<BUILD_INTERFACE:${IBKR_TWS_CLIENT_DIR}>
    $<BUILD_INTERFACE:${IBKR_TWS_PROTO_OUTPUT_DIR}>)
  if(TARGET protobuf::libprotobuf)
    target_link_libraries(ibkr_tws_api PUBLIC
      $<BUILD_INTERFACE:protobuf::libprotobuf>
      $<INSTALL_INTERFACE:protobuf::libprotobuf>)
  else()
    if(Protobuf_LIBRARY_RELEASE)
      set(_self_quant_protobuf_library "${Protobuf_LIBRARY_RELEASE}")
    else()
      set(_self_quant_protobuf_library "${Protobuf_LIBRARIES}")
    endif()
    add_library(self_quant_ibkr_protobuf UNKNOWN IMPORTED)
    set_target_properties(self_quant_ibkr_protobuf PROPERTIES
      IMPORTED_LOCATION "${_self_quant_protobuf_library}"
      INTERFACE_INCLUDE_DIRECTORIES "${Protobuf_INCLUDE_DIRS}")
    target_link_libraries(ibkr_tws_api PUBLIC
      $<BUILD_INTERFACE:self_quant_ibkr_protobuf>
      $<INSTALL_INTERFACE:protobuf::libprotobuf>)
  endif()
  add_library(self_quant_ibkr_libbid STATIC IMPORTED)
  set_target_properties(self_quant_ibkr_libbid PROPERTIES
    IMPORTED_LOCATION "${IBKR_DECIMAL_LIBRARY}")
  target_link_libraries(ibkr_tws_api PUBLIC
    $<BUILD_INTERFACE:self_quant_ibkr_libbid>
    $<INSTALL_INTERFACE:self_quant::ibkr_libbid>)
  target_compile_options(ibkr_tws_api PRIVATE -w)
  set(SELF_QUANT_IBKR_DECIMAL_LIBRARY "${IBKR_DECIMAL_LIBRARY}" PARENT_SCOPE)
endfunction()
