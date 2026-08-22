#include "mds/producer/producer_runtime.h"

#include <cassert>
#include <sstream>
#include <string>

int main() {
  std::ostringstream output;
  mds::producer::ProducerRuntime runtime({
      .config_path = MDS_PRODUCER_EXAMPLE_CONFIG,
      .validate_only = true,
      .output = &output,
  });
  const auto started = runtime.start();
  assert(started);
  assert(!runtime.failed());
  assert(runtime.error().empty());
  assert(!output.str().empty());
  assert(output.str().find("max_continuous_recovery_ms=300000") !=
         std::string::npos);
  assert(!runtime.resolved_segments().empty());
  assert(runtime.run_once(0) == 0);
  for (const auto &segment : runtime.resolved_segments()) {
    assert(!segment.name.empty());
    assert(segment.name.front() == '/');
    assert(segment.ring_bytes == 8U << 20U);
    assert(segment.max_record_bytes == 64U << 10U);
  }
  const auto duplicate_start = runtime.start();
  assert(!duplicate_start);
  assert(duplicate_start.error == mds::api::ErrorCode::AlreadyStarted);
  assert(!runtime.failed());
  runtime.stop();
  runtime.stop();
  assert(runtime.start());
  runtime.stop();

  std::ostringstream errors;
  mds::producer::ProducerRuntime invalid({
      .config_path = "/path/that/does/not/exist.yaml",
      .error_output = &errors,
  });
  const auto failed = invalid.start();
  assert(!failed);
  assert(failed.error == mds::api::ErrorCode::InvalidConfig);
  assert(invalid.failed());
  assert(!invalid.error().empty());
  assert(errors.str().find(invalid.error()) != std::string::npos);
  assert(invalid.resolved_segments().empty());

  mds::producer::ProducerRuntime not_started({
      .config_path = MDS_PRODUCER_EXAMPLE_CONFIG,
      .validate_only = true,
  });
  assert(not_started.run_once(0) == -1);
  assert(not_started.failed());
  assert(not_started.start());
  assert(!not_started.failed());
  not_started.stop();

  std::string create_error{"stale"};
  auto created = mds::producer::ProducerRuntime::create(
      MDS_PRODUCER_EXAMPLE_CONFIG, create_error);
  assert(created);
  assert(create_error.empty());
  created->stop();
  return 0;
}
