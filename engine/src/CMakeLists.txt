cmake_minimum_required(VERSION 3.13)
project(OrcaSlicer-native)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

add_subdirectory(dev-utils)

find_package(libnoise REQUIRED)

add_subdirectory(libslic3r)

if(ORCA_TOOLS)
    if(APPLE)
        add_executable(OrcaSlicer_profile_validator MACOSX_BUNDLE dev-utils/OrcaSlicer_profile_validator.cpp)
        set_target_properties(OrcaSlicer_profile_validator PROPERTIES
            MACOSX_BUNDLE TRUE
            MACOSX_BUNDLE_BUNDLE_NAME "OrcaSlicer Profile Validator"
            MACOSX_BUNDLE_BUNDLE_VERSION "${SLIC3R_VERSION}"
            MACOSX_BUNDLE_SHORT_VERSION_STRING "${SLIC3R_VERSION}"
            MACOSX_BUNDLE_IDENTIFIER "com.orcaslicer.OrcaSlicer.profile-validator"
            MACOSX_BUNDLE_COPYRIGHT "© 2026 OrcaSlicer Pte Ltd All Rights Reserved"
            MACOSX_BUNDLE_GUI_IDENTIFIER "com.orcaslicer.OrcaSlicer.profile-validator"
        )
    else()
        add_executable(OrcaSlicer_profile_validator dev-utils/OrcaSlicer_profile_validator.cpp)
    endif()
    target_link_libraries(OrcaSlicer_profile_validator libslic3r boost_headeronly libcurl OpenSSL::SSL OpenSSL::Crypto)
    target_compile_definitions(OrcaSlicer_profile_validator PRIVATE -DBOOST_ALL_NO_LIB -DBOOST_USE_WINAPI_VERSION=0x602 -DBOOST_SYSTEM_USE_UTF8)
endif()
