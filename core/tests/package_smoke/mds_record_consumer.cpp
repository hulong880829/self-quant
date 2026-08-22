#include "mds/record/record_reader.h"

#include <filesystem>

int main(int argc, char **argv) {
  if (argc == 2) {
    const auto result =
        mds::record::validate_file(std::filesystem::path(argv[1]));
    return result ? 0 : 1;
  }
  return 0;
}
