cmake_minimum_required(VERSION 3.16)
project(mrtranscribe C CXX)

# Static, portable build: no OpenMP, no GPU, no -march=native.
set(BUILD_SHARED_LIBS OFF CACHE BOOL "" FORCE)
set(WHISPER_BUILD_EXAMPLES OFF CACHE BOOL "" FORCE)
set(WHISPER_BUILD_TESTS OFF CACHE BOOL "" FORCE)
set(WHISPER_BUILD_SERVER OFF CACHE BOOL "" FORCE)
set(GGML_OPENMP OFF CACHE BOOL "" FORCE)
set(GGML_NATIVE OFF CACHE BOOL "" FORCE)
set(OPUS_BUILD_TESTING OFF CACHE BOOL "" FORCE)
set(OPUS_BUILD_PROGRAMS OFF CACHE BOOL "" FORCE)

add_subdirectory(vendor/opus EXCLUDE_FROM_ALL)
add_subdirectory(vendor/whisper.cpp EXCLUDE_FROM_ALL)

add_executable(mrtranscribe mrtranscribe.c)
target_link_libraries(mrtranscribe PRIVATE whisper opus)
target_link_options(mrtranscribe PRIVATE -static)
set_target_properties(mrtranscribe PROPERTIES LINKER_LANGUAGE CXX)
