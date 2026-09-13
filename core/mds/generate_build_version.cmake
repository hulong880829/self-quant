if(NOT DEFINED SOURCE_ROOT OR NOT DEFINED OUTPUT_FILE)
  message(FATAL_ERROR "SOURCE_ROOT and OUTPUT_FILE are required")
endif()

execute_process(
  COMMAND git rev-parse --short=12 HEAD
  WORKING_DIRECTORY "${SOURCE_ROOT}"
  OUTPUT_VARIABLE GIT_SHA
  OUTPUT_STRIP_TRAILING_WHITESPACE
  ERROR_QUIET
  RESULT_VARIABLE GIT_SHA_RESULT)
if(NOT GIT_SHA_RESULT EQUAL 0 OR GIT_SHA STREQUAL "")
  set(GIT_SHA "unknown")
endif()

execute_process(
  COMMAND git status --porcelain --untracked-files=normal
  WORKING_DIRECTORY "${SOURCE_ROOT}"
  OUTPUT_VARIABLE GIT_STATUS
  ERROR_QUIET
  RESULT_VARIABLE GIT_STATUS_RESULT)
if(GIT_STATUS_RESULT EQUAL 0 AND GIT_STATUS STREQUAL "")
  set(GIT_DIRTY "false")
else()
  set(GIT_DIRTY "true")
endif()

string(TIMESTAMP BUILD_UTC "%Y-%m-%dT%H:%M:%SZ" UTC)
string(TIMESTAMP BUILD_STAMP "%Y%m%dT%H%M%SZ" UTC)
string(RANDOM LENGTH 12 ALPHABET 0123456789abcdef BUILD_NONCE)
set(BUILD_ID "${GIT_SHA}-${BUILD_STAMP}-${BUILD_NONCE}")

get_filename_component(OUTPUT_DIRECTORY "${OUTPUT_FILE}" DIRECTORY)
file(MAKE_DIRECTORY "${OUTPUT_DIRECTORY}")
file(WRITE "${OUTPUT_FILE}"
"#include \"mds/producer/build_version.h\"\n"
"\n"
"namespace mds::producer {\n"
"\n"
"BuildVersion build_version() noexcept {\n"
"  return {\"${GIT_SHA}\", ${GIT_DIRTY}, \"${BUILD_UTC}\", \"${BUILD_ID}\"};\n"
"}\n"
"\n"
"}  // namespace mds::producer\n")
