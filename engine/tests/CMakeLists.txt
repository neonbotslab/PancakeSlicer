set(TEST_DATA_DIR ${CMAKE_CURRENT_SOURCE_DIR}/data)
file(TO_NATIVE_PATH "${TEST_DATA_DIR}" TEST_DATA_DIR)

set(PROFILES_DIR ${CMAKE_SOURCE_DIR}/resources/profiles)
file(TO_NATIVE_PATH "${PROFILES_DIR}" PROFILES_DIR)

add_library(test_common INTERFACE)
target_compile_definitions(test_common INTERFACE TEST_DATA_DIR=R"\(${TEST_DATA_DIR}\)" PROFILES_DIR=R"\(${PROFILES_DIR}\)" CATCH_CONFIG_FAST_COMPILE)
target_include_directories(test_common INTERFACE ${CMAKE_CURRENT_SOURCE_DIR})

if (APPLE)
    target_link_libraries(test_common INTERFACE "-liconv -framework IOKit" "-framework CoreFoundation" -lc++)
endif()

function(orcaslicer_copy_test_dlls)
    if (WIN32)
        set(_configs ${CMAKE_CONFIGURATION_TYPES})
        if (NOT _configs)
            set(_configs "${CMAKE_BUILD_TYPE}")
        endif()
        foreach(_cfg IN LISTS _configs)
            orcaslicer_copy_dlls("${_cfg}")
        endforeach()
    endif()
endfunction()

if (NOT BUILD_TESTS)
    add_subdirectory(fff_print)
    return()
endif()

add_subdirectory(catch2)

if (MSVC)
    target_compile_definitions(Catch2WithMain PRIVATE DO_NOT_USE_WMAIN)
endif()

include(CTest)
include(Catch)

set(CATCH_EXTRA_ARGS "" CACHE STRING "Extra arguments for catch2 test suites.")

function(orcaslicer_discover_tests TARGET)
    catch_discover_tests(${TARGET} ADD_TAGS_AS_LABELS)
endfunction()

add_subdirectory(libnest2d)
add_subdirectory(libslic3r)
add_subdirectory(fff_print)
add_subdirectory(sla_print)
add_subdirectory(filament_group)
