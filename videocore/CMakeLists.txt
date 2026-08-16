cmake_minimum_required(VERSION 3.25)

project(videocore LANGUAGES C CXX)

set(VC_FFMPEG_ROOT "" CACHE PATH
    "Absolute root of the repository-local FFmpeg MSVC SDK")

if(NOT IS_ABSOLUTE "${VC_FFMPEG_ROOT}")
  message(FATAL_ERROR "VC_FFMPEG_ROOT must be an absolute path")
endif()
get_filename_component(VC_FFMPEG_ROOT
  "${VC_FFMPEG_ROOT}" ABSOLUTE)

set(_vc_ffmpeg_libraries)
foreach(_vc_component IN ITEMS avformat avcodec avutil swscale)
  set(_vc_import_library
    "${VC_FFMPEG_ROOT}/lib/${_vc_component}.lib")
  if(NOT EXISTS "${_vc_import_library}")
    message(FATAL_ERROR
      "Missing FFmpeg import library: ${_vc_import_library}")
  endif()
  list(APPEND _vc_ffmpeg_libraries "${_vc_import_library}")

  file(GLOB _vc_component_dlls
    LIST_DIRECTORIES false
    "${VC_FFMPEG_ROOT}/bin/${_vc_component}-*.dll")
  list(LENGTH _vc_component_dlls _vc_component_dll_count)
  if(NOT _vc_component_dll_count EQUAL 1)
    message(FATAL_ERROR
      "Expected exactly one runtime DLL for ${_vc_component}")
  endif()
  list(APPEND _vc_ffmpeg_runtime_dlls ${_vc_component_dlls})
endforeach()

# avcodec-63.dll imports swresample-7.dll in this SDK.  Tests do not link
# swresample.lib, but the Windows loader still needs that transitive DLL.
file(GLOB _vc_swresample_runtime_dll
  LIST_DIRECTORIES false
  "${VC_FFMPEG_ROOT}/bin/swresample-*.dll")
list(LENGTH _vc_swresample_runtime_dll _vc_swresample_dll_count)
if(NOT _vc_swresample_dll_count EQUAL 1)
  message(FATAL_ERROR "Expected exactly one swresample runtime DLL")
endif()
list(APPEND _vc_ffmpeg_runtime_dlls ${_vc_swresample_runtime_dll})

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_C_EXTENSIONS OFF)
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_MSVC_RUNTIME_LIBRARY "MultiThreaded$<$<CONFIG:Debug>:Debug>")

enable_testing()

add_library(videocore SHARED
  src/api.cpp
  src/avio_bridge.cpp
  src/cancel_token.cpp
  src/contact_sheet.cpp
  src/deadline.cpp
  src/error.cpp
  src/image_analysis.cpp
  src/media_session.cpp
  src/native_algorithms/sha512.cpp
  src/runtime_info.cpp
  src/video_analysis.cpp
  src/win_file.cpp
  exports.def
)
target_compile_definitions(videocore PRIVATE VC_BUILDING_DLL)
target_compile_features(videocore PRIVATE cxx_std_17)
target_compile_options(videocore PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_include_directories(videocore
  PUBLIC
    "${CMAKE_CURRENT_SOURCE_DIR}/include"
  PRIVATE
    "${CMAKE_CURRENT_SOURCE_DIR}/src"
    "${VC_FFMPEG_ROOT}/include"
)
target_link_libraries(videocore PRIVATE
  ${_vc_ffmpeg_libraries}
)
set_target_properties(videocore PROPERTIES
  OUTPUT_NAME "videocore"
)

add_executable(test_vc_abi
  tests/test_abi.cpp
  src/error.cpp
)
target_compile_features(test_vc_abi PRIVATE cxx_std_17)
target_compile_options(test_vc_abi PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
)
target_include_directories(test_vc_abi PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/include"
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
)
target_link_libraries(test_vc_abi PRIVATE videocore)

add_test(NAME videocore_abi COMMAND test_vc_abi)

add_executable(test_vc_runtime
  tests/test_runtime_info.cpp
  src/error.cpp
  src/runtime_info.cpp
)
target_compile_features(test_vc_runtime PRIVATE cxx_std_17)
target_compile_options(test_vc_runtime PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_include_directories(test_vc_runtime PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/include"
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
  "${VC_FFMPEG_ROOT}/include"
)
target_link_libraries(test_vc_runtime PRIVATE videocore)
target_link_libraries(test_vc_runtime PRIVATE
  ${_vc_ffmpeg_libraries}
)
add_test(NAME videocore_runtime COMMAND test_vc_runtime)

add_executable(test_vc_cancel
  tests/test_cancel.cpp
  src/cancel_token.cpp
  src/deadline.cpp
  src/error.cpp
)
target_compile_features(test_vc_cancel PRIVATE cxx_std_17)
target_compile_definitions(test_vc_cancel PRIVATE
  VC_CANCEL_TESTING
)
target_compile_options(test_vc_cancel PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_include_directories(test_vc_cancel PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/include"
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
)
target_link_libraries(test_vc_cancel PRIVATE videocore)
add_test(NAME videocore_cancel COMMAND test_vc_cancel)

add_executable(test_vc_session
  tests/test_media_session.cpp
  src/api.cpp
  src/avio_bridge.cpp
  src/cancel_token.cpp
  src/deadline.cpp
  src/error.cpp
  src/media_session.cpp
  src/native_algorithms/sha512.cpp
  src/runtime_info.cpp
  src/win_file.cpp
)
target_compile_features(test_vc_session PRIVATE cxx_std_17)
target_compile_definitions(test_vc_session PRIVATE
  VC_BUILDING_DLL
  VC_MEDIA_SESSION_TESTING
  VC_WIN_FILE_TESTING
)
target_compile_options(test_vc_session PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_include_directories(test_vc_session PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/include"
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
  "${VC_FFMPEG_ROOT}/include"
)
target_link_libraries(test_vc_session PRIVATE
  ${_vc_ffmpeg_libraries}
)
add_test(NAME videocore_session COMMAND test_vc_session)

add_executable(test_vc_image_analysis
  tests/test_image_analysis.cpp
  src/api.cpp
  src/avio_bridge.cpp
  src/cancel_token.cpp
  src/deadline.cpp
  src/error.cpp
  src/media_session.cpp
  src/native_algorithms/sha512.cpp
  src/runtime_info.cpp
  src/win_file.cpp
)
target_compile_features(test_vc_image_analysis PRIVATE cxx_std_17)
target_compile_definitions(test_vc_image_analysis PRIVATE
  VC_BUILDING_DLL
  VC_IMAGE_ANALYSIS_TESTING
  VC_MEDIA_SESSION_TESTING
  VC_WIN_FILE_TESTING
)
target_compile_options(test_vc_image_analysis PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_include_directories(test_vc_image_analysis PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/include"
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
  "${VC_FFMPEG_ROOT}/include"
)
target_link_libraries(test_vc_image_analysis PRIVATE
  ${_vc_ffmpeg_libraries}
)
add_test(NAME videocore_image_analysis COMMAND test_vc_image_analysis)

add_executable(test_vc_video_analysis
  tests/test_video_analysis.cpp
  src/api.cpp
  src/avio_bridge.cpp
  src/cancel_token.cpp
  src/deadline.cpp
  src/error.cpp
  src/media_session.cpp
  src/native_algorithms/sha512.cpp
  src/runtime_info.cpp
  src/win_file.cpp
)
target_compile_features(test_vc_video_analysis PRIVATE cxx_std_17)
target_compile_definitions(test_vc_video_analysis PRIVATE
  VC_BUILDING_DLL
  VC_MEDIA_SESSION_TESTING
  VC_VIDEO_ANALYSIS_TESTING
  VC_WIN_FILE_TESTING
  VC_VIDEO_TESTDATA_ROOT=L"${CMAKE_CURRENT_SOURCE_DIR}/testdata/video"
)
target_compile_options(test_vc_video_analysis PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_include_directories(test_vc_video_analysis PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/include"
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
  "${VC_FFMPEG_ROOT}/include"
)
target_link_libraries(test_vc_video_analysis PRIVATE
  ${_vc_ffmpeg_libraries}
)
add_test(NAME videocore_video_analysis COMMAND test_vc_video_analysis)

add_executable(test_vc_contact_sheet
  tests/test_contact_sheet.cpp
  src/api.cpp
  src/avio_bridge.cpp
  src/cancel_token.cpp
  src/deadline.cpp
  src/error.cpp
  src/media_session.cpp
  src/native_algorithms/sha512.cpp
  src/runtime_info.cpp
  src/win_file.cpp
)
target_compile_features(test_vc_contact_sheet PRIVATE cxx_std_17)
target_compile_options(test_vc_contact_sheet PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_compile_definitions(test_vc_contact_sheet PRIVATE
  VC_BUILDING_DLL
  VC_MEDIA_SESSION_TESTING
  VC_VIDEO_ANALYSIS_TESTING
  VC_WIN_FILE_TESTING
  VC_VIDEO_TESTDATA_ROOT=L"${CMAKE_CURRENT_SOURCE_DIR}/testdata/video"
)
target_include_directories(test_vc_contact_sheet PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/include"
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
  "${VC_FFMPEG_ROOT}/include"
)
target_link_libraries(test_vc_contact_sheet PRIVATE
  ${_vc_ffmpeg_libraries}
  bcrypt
)
add_test(NAME videocore_contact_sheet COMMAND test_vc_contact_sheet)

add_executable(test_vc_resilience
  tests/test_resilience.cpp
  src/api.cpp
  src/avio_bridge.cpp
  src/cancel_token.cpp
  src/deadline.cpp
  src/error.cpp
  src/media_session.cpp
  src/native_algorithms/sha512.cpp
  src/runtime_info.cpp
  src/win_file.cpp
)
target_compile_features(test_vc_resilience PRIVATE cxx_std_17)
target_compile_options(test_vc_resilience PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_compile_definitions(test_vc_resilience PRIVATE
  VC_BUILDING_DLL
  VC_MEDIA_SESSION_TESTING
  VC_VIDEO_ANALYSIS_TESTING
  VC_WIN_FILE_TESTING
  VC_RESILIENCE_TESTING
  VC_VIDEO_TESTDATA_ROOT=L"${CMAKE_CURRENT_SOURCE_DIR}/testdata/video"
)
target_include_directories(test_vc_resilience PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/include"
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
  "${VC_FFMPEG_ROOT}/include"
)
target_link_libraries(test_vc_resilience PRIVATE
  ${_vc_ffmpeg_libraries}
  bcrypt
)
add_test(NAME videocore_resilience COMMAND test_vc_resilience)

add_executable(test_vc_video_sar_reference
  tests/test_video_sar_reference.cpp
)
target_compile_features(test_vc_video_sar_reference PRIVATE cxx_std_17)
target_compile_options(test_vc_video_sar_reference PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_include_directories(test_vc_video_sar_reference PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
  "${VC_FFMPEG_ROOT}/include"
)
target_link_libraries(test_vc_video_sar_reference PRIVATE
  ${_vc_ffmpeg_libraries}
  bcrypt
)

get_filename_component(_vc_compiler_directory
  "${CMAKE_CXX_COMPILER}" DIRECTORY)
set(_vc_dumpbin
  "${_vc_compiler_directory}/dumpbin.exe")
if(NOT EXISTS "${_vc_dumpbin}")
  message(FATAL_ERROR "dumpbin.exe is required for production isolation")
endif()
set(_vc_vsdevcmd
  "${CMAKE_GENERATOR_INSTANCE}/Common7/Tools/VsDevCmd.bat")
if(NOT EXISTS "${_vc_vsdevcmd}")
  message(FATAL_ERROR "VsDevCmd.bat is required for production isolation")
endif()
find_program(_vc_powershell
  NAMES pwsh.exe powershell.exe
  REQUIRED)
add_test(
  NAME videocore_video_legacy_golden
  COMMAND "${_vc_powershell}" -NoProfile
    -File "${CMAKE_CURRENT_SOURCE_DIR}/tests/test_video_legacy_golden.ps1"
    -Executable "$<TARGET_FILE:test_vc_video_analysis>"
    -SarReference "$<TARGET_FILE:test_vc_video_sar_reference>"
    -SarFixture "${CMAKE_CURRENT_SOURCE_DIR}/testdata/video/h264-sar-4x3.mp4"
    -Golden "${CMAKE_CURRENT_SOURCE_DIR}/../testdata/videocore/compat/legacy-golden.json"
)
add_test(
  NAME videocore_video_fixture_integrity
  COMMAND "${_vc_powershell}" -NoProfile
    -File "${CMAKE_CURRENT_SOURCE_DIR}/tests/test_video_fixture_integrity.ps1"
    -SourceRoot "${CMAKE_CURRENT_SOURCE_DIR}/../testdata/videocore/compat/videos"
    -CopyRoot "${CMAKE_CURRENT_SOURCE_DIR}/testdata/video"
)
add_test(
  NAME videocore_session_production_isolation
  COMMAND "${_vc_powershell}" -NoProfile
    -File "${CMAKE_CURRENT_SOURCE_DIR}/tests/test_production_isolation.ps1"
    -Dumpbin "${_vc_dumpbin}"
    -WinFileObject
      "${CMAKE_CURRENT_BINARY_DIR}/videocore.dir/$<CONFIG>/win_file.obj"
    -MediaSessionObject
      "${CMAKE_CURRENT_BINARY_DIR}/videocore.dir/$<CONFIG>/media_session.obj"
    -Compiler "${CMAKE_CXX_COMPILER}"
    -VsDevCmd "${_vc_vsdevcmd}"
    -CompilerCommandTlog
      "${CMAKE_CURRENT_BINARY_DIR}/videocore.dir/$<CONFIG>/videocore.tlog/CL.command.1.tlog"
    -WinFileSource
      "${CMAKE_CURRENT_SOURCE_DIR}/src/win_file.cpp"
    -MediaSessionSource
      "${CMAKE_CURRENT_SOURCE_DIR}/src/media_session.cpp"
    -MediaSessionHeader
      "${CMAKE_CURRENT_SOURCE_DIR}/src/media_session.h"
)

add_executable(test_vc_unicode
  tests/test_unicode_paths.cpp
)
target_compile_features(test_vc_unicode PRIVATE cxx_std_17)
target_compile_options(test_vc_unicode PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
)
target_include_directories(test_vc_unicode PRIVATE
  "${CMAKE_CURRENT_SOURCE_DIR}/include"
  "${CMAKE_CURRENT_SOURCE_DIR}/src"
)
target_link_libraries(test_vc_unicode PRIVATE videocore)
add_test(NAME videocore_unicode COMMAND test_vc_unicode)

set(_vc_image_algorithm_sources
  src/native_algorithms/image_decode.cpp
  src/native_algorithms/pdq.cpp
  src/native_algorithms/phash_parts.cpp
  src/native_algorithms/sobel_hist.cpp
  src/native_algorithms/stb_impl.cpp
)
set(_vc_image_algorithms_present TRUE)
foreach(_vc_image_source IN LISTS _vc_image_algorithm_sources)
  if(NOT EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/${_vc_image_source}")
    set(_vc_image_algorithms_present FALSE)
  endif()
endforeach()

add_executable(test_vc_image_compat
  tests/test_image_compat.cpp
)
target_compile_features(test_vc_image_compat PRIVATE cxx_std_17)
target_compile_options(test_vc_image_compat PRIVATE
  $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
  $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
  $<$<CXX_COMPILER_ID:MSVC>:/fp:precise>
)
target_compile_definitions(test_vc_image_compat PRIVATE
  VC_COMPAT_ROOT="${CMAKE_CURRENT_SOURCE_DIR}/../testdata/videocore/compat"
  VC_IMAGE_TESTDATA_ROOT="${CMAKE_CURRENT_SOURCE_DIR}/testdata"
  VC_LEVEL_B_LEGACY_GOLDEN="${CMAKE_CURRENT_SOURCE_DIR}/testdata/legacy_level_b/legacy-golden.tsv"
)
if(_vc_image_algorithms_present)
  find_package(libjpeg-turbo CONFIG REQUIRED)
  find_package(PNG REQUIRED)
  find_package(WebP CONFIG REQUIRED)
  file(GLOB _vc_pdq_sources CONFIGURE_DEPENDS
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream/pdq/cpp/common/*.cpp"
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream/pdq/cpp/downscaling/*.cpp"
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream/pdq/cpp/hashing/*.cpp"
  )
  add_library(videocore_image_algorithms OBJECT
    ${_vc_image_algorithm_sources}
    ${_vc_pdq_sources}
  )
  file(TO_NATIVE_PATH
    "${CMAKE_CURRENT_BINARY_DIR}/videocore_image_algorithms.dir"
    _vc_image_object_native_dir)
  file(TO_NATIVE_PATH "${CMAKE_CURRENT_SOURCE_DIR}"
    _vc_source_native_dir)
  foreach(_vc_image_source IN LISTS _vc_image_algorithm_sources _vc_pdq_sources)
    if(IS_ABSOLUTE "${_vc_image_source}")
      set(_vc_image_source_abs "${_vc_image_source}")
    else()
      set(_vc_image_source_abs "${CMAKE_CURRENT_SOURCE_DIR}/${_vc_image_source}")
    endif()
    get_filename_component(_vc_image_object_id "${_vc_image_source_abs}" NAME_WE)
    set_property(SOURCE "${_vc_image_source_abs}" APPEND PROPERTY VS_SETTINGS
      "ObjectFileName=videocore_image_algorithms.dir\\$(Configuration)\\${_vc_image_object_id}.obj"
      "ProgramDataBaseFileName=videocore_image_algorithms.dir\\$(Configuration)\\videocore_image_algorithms.pdb")
  endforeach()
  target_compile_features(videocore_image_algorithms PRIVATE cxx_std_17)
  # The deterministic provenance gate replays each CL.command record as one
  # translation unit.  Prevent the Visual Studio generator from batching
  # multiple sources into one cl.exe process, which otherwise changes COFF
  # deterministic IDs even with /Brepro.
  set_target_properties(videocore_image_algorithms PROPERTIES
    VS_NO_COMPILE_BATCHING ON
  )
  string(TOUPPER "${_vc_image_object_native_dir}"
    _vc_image_object_native_dir_upper)
  target_compile_options(videocore_image_algorithms PRIVATE
    $<$<CXX_COMPILER_ID:MSVC>:/EHsc>
    $<$<CXX_COMPILER_ID:MSVC>:/utf-8>
    $<$<CXX_COMPILER_ID:MSVC>:/fp:precise>
    $<$<CXX_COMPILER_ID:MSVC>:/Brepro>
    $<$<CXX_COMPILER_ID:MSVC>:/experimental:deterministic>
    $<$<CXX_COMPILER_ID:MSVC>:/pathmap:${_vc_image_object_native_dir_upper}\\$<UPPER_CASE:$<CONFIG>>=VC_IMAGE_OBJECTS>
    $<$<CXX_COMPILER_ID:MSVC>:/pathmap:${_vc_source_native_dir}=VC_SOURCE_ROOT>
  )
  target_compile_definitions(videocore_image_algorithms PRIVATE
    NOMINMAX
    WIN32_LEAN_AND_MEAN
  )
  target_include_directories(videocore_image_algorithms PRIVATE
    "${CMAKE_CURRENT_SOURCE_DIR}/src"
    "${CMAKE_CURRENT_SOURCE_DIR}/src/pdq_upstream"
    "${CMAKE_CURRENT_SOURCE_DIR}/third_party"
  )
  target_link_libraries(videocore_image_algorithms PUBLIC
    "$<IF:$<TARGET_EXISTS:libjpeg-turbo::turbojpeg>,libjpeg-turbo::turbojpeg,libjpeg-turbo::turbojpeg-static>"
    PNG::PNG
    WebP::webp
  )
  target_compile_definitions(test_vc_image_compat PRIVATE
    VC_IMAGE_ALGORITHMS_PRESENT=1
  )
  target_include_directories(test_vc_image_compat PRIVATE
    "${CMAKE_CURRENT_SOURCE_DIR}/src"
  )
  target_link_libraries(test_vc_image_compat PRIVATE
    videocore_image_algorithms
  )
  target_link_libraries(videocore PRIVATE
    videocore_image_algorithms
  )
  target_link_options(videocore PRIVATE
    $<$<CXX_COMPILER_ID:MSVC>:/Brepro>
    $<$<CXX_COMPILER_ID:MSVC>:/INCREMENTAL:NO>
  )
  target_link_options(test_vc_image_compat PRIVATE
    $<$<CXX_COMPILER_ID:MSVC>:/Brepro>
    $<$<CXX_COMPILER_ID:MSVC>:/INCREMENTAL:NO>
  )
endif()
target_sources(test_vc_image_analysis PRIVATE src/image_analysis.cpp)
target_sources(test_vc_image_analysis PRIVATE src/video_analysis.cpp)
target_sources(test_vc_image_analysis PRIVATE src/contact_sheet.cpp)
target_link_libraries(test_vc_image_analysis PRIVATE videocore_image_algorithms)
target_sources(test_vc_session PRIVATE src/image_analysis.cpp)
target_sources(test_vc_session PRIVATE src/video_analysis.cpp)
target_sources(test_vc_session PRIVATE src/contact_sheet.cpp)
target_link_libraries(test_vc_session PRIVATE videocore_image_algorithms)
target_sources(test_vc_video_analysis PRIVATE src/image_analysis.cpp)
target_sources(test_vc_video_analysis PRIVATE src/video_analysis.cpp)
target_sources(test_vc_video_analysis PRIVATE src/contact_sheet.cpp)
target_link_libraries(test_vc_video_analysis PRIVATE videocore_image_algorithms)
target_sources(test_vc_contact_sheet PRIVATE src/image_analysis.cpp)
target_sources(test_vc_contact_sheet PRIVATE src/video_analysis.cpp)
target_sources(test_vc_contact_sheet PRIVATE src/contact_sheet.cpp)
target_link_libraries(test_vc_contact_sheet PRIVATE videocore_image_algorithms)
target_sources(test_vc_resilience PRIVATE src/image_analysis.cpp)
target_sources(test_vc_resilience PRIVATE src/video_analysis.cpp)
target_sources(test_vc_resilience PRIVATE src/contact_sheet.cpp)
target_link_libraries(test_vc_resilience PRIVATE videocore_image_algorithms)
target_link_libraries(test_vc_video_sar_reference PRIVATE videocore_image_algorithms)
add_test(NAME videocore_image_compat COMMAND test_vc_image_compat)

if(NOT IS_ABSOLUTE "${VCPKG_INSTALLED_DIR}")
  message(FATAL_ERROR
    "VCPKG_INSTALLED_DIR must be an absolute path for provenance tests")
endif()
get_filename_component(_vc_vcpkg_installed_dir
  "${VCPKG_INSTALLED_DIR}" ABSOLUTE)
if("${VCPKG_TARGET_TRIPLET}" STREQUAL "" OR
   VCPKG_TARGET_TRIPLET MATCHES "[/\\\\]")
  message(FATAL_ERROR
    "VCPKG_TARGET_TRIPLET must be a non-empty path segment for provenance tests")
endif()
set(_vc_vcpkg_target_triplet "${VCPKG_TARGET_TRIPLET}")

add_test(
  NAME videocore_image_object_provenance
  COMMAND "${_vc_powershell}" -NoProfile
    -File "${CMAKE_CURRENT_SOURCE_DIR}/tests/test_image_object_provenance.ps1"
    -Dumpbin "${_vc_dumpbin}"
    -SourceRoot "${CMAKE_CURRENT_SOURCE_DIR}"
    -LegacySourceRoot "${CMAKE_CURRENT_SOURCE_DIR}/../mediacore"
    -ImageObjectDir
      "${CMAKE_CURRENT_BINARY_DIR}/videocore_image_algorithms.dir/$<CONFIG>"
    -DllTargetDir
      "${CMAKE_CURRENT_BINARY_DIR}/videocore.dir/$<CONFIG>"
    -CompatTargetDir
      "${CMAKE_CURRENT_BINARY_DIR}/test_vc_image_compat.dir/$<CONFIG>"
    -DllBinary "$<TARGET_FILE:videocore>"
    -CompatBinary "$<TARGET_FILE:test_vc_image_compat>"
    -Compiler "${CMAKE_CXX_COMPILER}"
    -VsDevCmd "${_vc_vsdevcmd}"
    -CompilerCommandTlog
      "${CMAKE_CURRENT_BINARY_DIR}/videocore_image_algorithms.dir/$<CONFIG>"
    -CanonicalImageObjectDir
      "${CMAKE_CURRENT_BINARY_DIR}/videocore_image_algorithms.dir/$<CONFIG>"
    -BuildRoot "${CMAKE_CURRENT_BINARY_DIR}"
    -VcpkgInstalledDir "${_vc_vcpkg_installed_dir}"
    -VcpkgTargetTriplet "${_vc_vcpkg_target_triplet}"
)

add_test(
  NAME videocore_image_object_provenance_mutation
  COMMAND "${_vc_powershell}" -NoProfile
    -File "${CMAKE_CURRENT_SOURCE_DIR}/tests/test_image_object_provenance_mutation.ps1"
    -GateScript "${CMAKE_CURRENT_SOURCE_DIR}/tests/test_image_object_provenance.ps1"
    -CurrentImageObjectDir
      "${CMAKE_CURRENT_BINARY_DIR}/videocore_image_algorithms.dir/$<CONFIG>"
    -CurrentDllBinary "$<TARGET_FILE:videocore>"
    -CurrentCompatBinary "$<TARGET_FILE:test_vc_image_compat>"
    -Dumpbin "${_vc_dumpbin}"
    -SourceRoot "${CMAKE_CURRENT_SOURCE_DIR}"
    -Compiler "${CMAKE_CXX_COMPILER}"
    -VsDevCmd "${_vc_vsdevcmd}"
    -CompilerCommandTlog
      "${CMAKE_CURRENT_BINARY_DIR}/videocore_image_algorithms.dir/$<CONFIG>"
    -BuildRoot "${CMAKE_CURRENT_BINARY_DIR}"
    -VcpkgInstalledDir "${_vc_vcpkg_installed_dir}"
    -VcpkgTargetTriplet "${_vc_vcpkg_target_triplet}"
)

set(_vc_dumpbin "${CMAKE_DUMPBIN}")
if(NOT _vc_dumpbin)
  get_filename_component(_vc_compiler_directory
    "${CMAKE_CXX_COMPILER}" DIRECTORY)
  set(_vc_dumpbin "${_vc_compiler_directory}/dumpbin.exe")
endif()
if(NOT EXISTS "${_vc_dumpbin}")
  message(FATAL_ERROR
    "dumpbin.exe is required for image production isolation: ${_vc_dumpbin}")
endif()

add_test(
  NAME videocore_image_production_isolation
  COMMAND "${_vc_powershell}" -NoProfile
    -File "${CMAKE_CURRENT_SOURCE_DIR}/tests/test_image_production_isolation.ps1"
    -SourceFile
      "${CMAKE_CURRENT_SOURCE_DIR}/src/native_algorithms/image_decode.cpp"
    -HeaderFile
      "${CMAKE_CURRENT_SOURCE_DIR}/src/native_algorithms/image_decode.h"
    -ObjectFile
      "${CMAKE_CURRENT_BINARY_DIR}/videocore_image_algorithms.dir/$<CONFIG>/image_decode.obj"
    -Dumpbin
      "${_vc_dumpbin}"
)

# These pins are the independently reviewed output of the one-time isolated
# legacy capture.  They intentionally live outside the artifact they anchor.
set(_vc_level_b_manifest_sha256
  "9A552825A4CF493A1407063CD3702176AF584D3D728780F476A1EB3CAA9EE1E3")
set(_vc_level_b_golden_sha256
  "95E019F0ADB796DBEA76AB341D5F76417A5653D444804626593CACFFC2D01517")

add_test(
  NAME videocore_level_b_legacy_artifact
  COMMAND "${_vc_powershell}" -NoProfile
    -File "${CMAKE_CURRENT_SOURCE_DIR}/../scripts/verify_videocore_level_b_legacy.ps1"
    -ArtifactDir "${CMAKE_CURRENT_SOURCE_DIR}/testdata/legacy_level_b"
    -InputRoot "${CMAKE_CURRENT_SOURCE_DIR}/testdata/level_b"
    -ExpectedManifestSha256
      "${_vc_level_b_manifest_sha256}"
    -ExpectedGoldenSha256
      "${_vc_level_b_golden_sha256}"
)

add_test(
  NAME videocore_level_b_legacy_mutation
  COMMAND "${_vc_powershell}" -NoProfile
    -File "${CMAKE_CURRENT_SOURCE_DIR}/tests/test_level_b_artifact_mutation.ps1"
    -Verifier "${CMAKE_CURRENT_SOURCE_DIR}/../scripts/verify_videocore_level_b_legacy.ps1"
    -ArtifactDir "${CMAKE_CURRENT_SOURCE_DIR}/testdata/legacy_level_b"
    -InputRoot "${CMAKE_CURRENT_SOURCE_DIR}/testdata/level_b"
    -ExpectedManifestSha256
      "${_vc_level_b_manifest_sha256}"
    -ExpectedGoldenSha256
      "${_vc_level_b_golden_sha256}"
)

foreach(_vc_test_target IN ITEMS
    test_vc_abi
    test_vc_runtime
    test_vc_cancel
    test_vc_image_analysis
    test_vc_video_analysis
    test_vc_contact_sheet
    test_vc_video_sar_reference
    test_vc_resilience
    test_vc_session
    test_vc_unicode
    test_vc_image_compat)
  foreach(_vc_runtime_dll IN LISTS _vc_ffmpeg_runtime_dlls)
    add_custom_command(TARGET ${_vc_test_target} POST_BUILD
      COMMAND "${CMAKE_COMMAND}" -E copy_if_different
        "${_vc_runtime_dll}"
        "$<TARGET_FILE_DIR:${_vc_test_target}>"
      VERBATIM
    )
  endforeach()
endforeach()

include(CheckCSourceCompiles)
set(_vc_saved_try_compile_target_type "${CMAKE_TRY_COMPILE_TARGET_TYPE}")
set(CMAKE_TRY_COMPILE_TARGET_TYPE STATIC_LIBRARY)
set(CMAKE_REQUIRED_INCLUDES "${CMAKE_CURRENT_SOURCE_DIR}/include")
check_c_source_compiles([=[
  #include <stddef.h>
  #include <videocore/videocore.h>
  _Static_assert(VC_ABI_VERSION == 1u, "ABI version");
  _Static_assert(sizeof(struct vc_runtime_info) == 136u, "runtime layout");
  _Static_assert(offsetof(struct vc_runtime_info, struct_size) == 0u,
                 "runtime struct_size");
  _Static_assert(offsetof(struct vc_runtime_info, abi_version) == 4u,
                 "runtime abi_version");
  static int32_t (VC_CALL *runtime_info_fn)(
      struct vc_runtime_info*, vc_error*) = &vc_runtime_info;
  int vc_header_c11_probe(void) {
      return runtime_info_fn == 0;
  }
]=] VC_PUBLIC_HEADER_COMPILES_AS_C11)
set(CMAKE_REQUIRED_INCLUDES)
if(_vc_saved_try_compile_target_type)
  set(CMAKE_TRY_COMPILE_TARGET_TYPE "${_vc_saved_try_compile_target_type}")
else()
  unset(CMAKE_TRY_COMPILE_TARGET_TYPE)
endif()
if(NOT VC_PUBLIC_HEADER_COMPILES_AS_C11)
  message(FATAL_ERROR "Public VideoCore header must compile as C11")
endif()
