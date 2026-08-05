cmake_minimum_required(VERSION 3.20)

project(mediacore VERSION 1.0.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_MSVC_RUNTIME_LIBRARY "MultiThreaded$<$<CONFIG:Debug>:Debug>")

find_package(libjpeg-turbo CONFIG REQUIRED)
find_package(PNG REQUIRED)
find_package(WebP CONFIG REQUIRED)

file(GLOB PDQ_SOURCES CONFIGURE_DEPENDS
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream/pdq/cpp/common/*.cpp"
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream/pdq/cpp/downscaling/*.cpp"
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream/pdq/cpp/hashing/*.cpp"
)

add_library(mediacore SHARED
    src/api.cpp
    src/phash_parts.cpp
    src/pdq_api.cpp
    src/sobel_hist.cpp
    src/stb_impl.cpp
    ${PDQ_SOURCES}
    exports.def
)
target_compile_definitions(mediacore PRIVATE
    MEDIACORE_BUILD
    NOMINMAX
    WIN32_LEAN_AND_MEAN
)
target_compile_options(mediacore PRIVATE
    $<$<CXX_COMPILER_ID:MSVC>:/fp:precise>
)
target_include_directories(mediacore
    PUBLIC
        "${CMAKE_CURRENT_SOURCE_DIR}/include"
    PRIVATE
        "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream"
        "${CMAKE_CURRENT_SOURCE_DIR}/third_party"
)
target_link_libraries(mediacore PRIVATE
    "$<IF:$<TARGET_EXISTS:libjpeg-turbo::turbojpeg>,libjpeg-turbo::turbojpeg,libjpeg-turbo::turbojpeg-static>"
    PNG::PNG
    WebP::webp
    bcrypt
)
if(MSVC)
    target_link_options(mediacore PRIVATE
        "/DEF:${CMAKE_CURRENT_SOURCE_DIR}/exports.def"
    )
endif()
set_target_properties(mediacore PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
    ARCHIVE_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
    LIBRARY_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)

enable_testing()
add_executable(test_sha512 tests/test_sha512.cpp)
target_link_libraries(test_sha512 PRIVATE mediacore)
set_target_properties(test_sha512 PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)
add_test(NAME mediacore_sha512 COMMAND test_sha512)

add_executable(test_api_internal tests/test_api_internal.cpp)
target_sources(test_api_internal PRIVATE
    src/pdq_api.cpp
    src/stb_impl.cpp
    ${PDQ_SOURCES}
)
target_compile_definitions(test_api_internal PRIVATE
    NOMINMAX
    WIN32_LEAN_AND_MEAN
)
target_include_directories(test_api_internal PRIVATE
    "${CMAKE_CURRENT_SOURCE_DIR}/include"
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream"
    "${CMAKE_CURRENT_SOURCE_DIR}/third_party"
)
target_link_libraries(test_api_internal PRIVATE
    "$<IF:$<TARGET_EXISTS:libjpeg-turbo::turbojpeg>,libjpeg-turbo::turbojpeg,libjpeg-turbo::turbojpeg-static>"
    PNG::PNG
    WebP::webp
)
set_target_properties(test_api_internal PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)
add_test(NAME mediacore_sha512_internal COMMAND test_api_internal)

add_executable(test_image tests/test_image.cpp)
target_include_directories(test_image PRIVATE
    "${CMAKE_CURRENT_SOURCE_DIR}/include"
)
target_link_libraries(test_image PRIVATE
    mediacore
    "$<IF:$<TARGET_EXISTS:libjpeg-turbo::turbojpeg>,libjpeg-turbo::turbojpeg,libjpeg-turbo::turbojpeg-static>"
    PNG::PNG
    WebP::webp
)
set_target_properties(test_image PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)
add_test(NAME mediacore_image COMMAND test_image)

add_executable(test_phase2 tests/test_phase2.cpp)
target_include_directories(test_phase2 PRIVATE
    "${CMAKE_CURRENT_SOURCE_DIR}/include"
)
target_link_libraries(test_phase2 PRIVATE mediacore)
set_target_properties(test_phase2 PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)
add_test(NAME mediacore_phase2 COMMAND test_phase2)

add_executable(mc_make_luma tests/make_luma.cpp)
set_target_properties(mc_make_luma PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)

add_executable(mc_luma_runner tests/mc_luma_runner.cpp)
target_link_libraries(mc_luma_runner PRIVATE mediacore)
set_target_properties(mc_luma_runner PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)

add_executable(ref_luma_hasher
    tests/ref_luma_main.cpp
    ${PDQ_SOURCES}
)
target_include_directories(ref_luma_hasher PRIVATE
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream"
)
target_compile_options(ref_luma_hasher PRIVATE
    $<$<CXX_COMPILER_ID:MSVC>:/fp:precise>
)
set_target_properties(ref_luma_hasher PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)

add_executable(mc_endtoend tests/endtoend_main.cpp)
target_link_libraries(mc_endtoend PRIVATE mediacore)
set_target_properties(mc_endtoend PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)

add_executable(mc_make_level_b
    tests/make_level_b.cpp
    ${PDQ_SOURCES}
)
target_include_directories(mc_make_level_b PRIVATE
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream"
)
target_compile_definitions(mc_make_level_b PRIVATE
    NOMINMAX
    WIN32_LEAN_AND_MEAN
)
target_link_libraries(mc_make_level_b PRIVATE
    "$<IF:$<TARGET_EXISTS:libjpeg-turbo::turbojpeg>,libjpeg-turbo::turbojpeg,libjpeg-turbo::turbojpeg-static>"
    PNG::PNG
    WebP::webp
    windowscodecs
    ole32
)
target_compile_options(mc_make_level_b PRIVATE
    $<$<CXX_COMPILER_ID:MSVC>:/fp:precise>
)
set_target_properties(mc_make_level_b PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_BINARY_DIR}/Release"
)
