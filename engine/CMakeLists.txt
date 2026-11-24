if(${CMAKE_VERSION} VERSION_GREATER_EQUAL "4.0")
    set(CMAKE_POLICY_VERSION_MINIMUM 3.13 CACHE STRING "" FORCE)
endif()

cmake_minimum_required(VERSION 3.13)

if(POLICY CMP0177)
    cmake_policy(SET CMP0177 NEW)
endif()


set_property(GLOBAL PROPERTY USE_FOLDERS ON)

if (WIN32)
    set(IS_CI FALSE)
    if(DEFINED ENV{CI})
        set(IS_CI TRUE)
    elseif(DEFINED ENV{GITHUB_ACTIONS})
        set(IS_CI TRUE)
    elseif(DEFINED ENV{GITLAB_CI})
        set(IS_CI TRUE)
    elseif(DEFINED ENV{TF_BUILD})
        set(IS_CI TRUE)
    elseif(DEFINED ENV{BUILD_NUMBER})
        set(IS_CI TRUE)
    endif()

    string(REPLACE "\\" "/" ENV_PATH "$ENV{PATH}")
    string(FIND "${ENV_PATH}" "Strawberry/c/bin" STRAWBERRY_POS)
    string(FIND "${ENV_PATH}" "Program Files/CMake/bin" CMAKE_POS)

    if (STRAWBERRY_POS GREATER -1 AND CMAKE_POS GREATER -1 AND STRAWBERRY_POS LESS CMAKE_POS)
        set(_warning_text "
#############################################################
Detected Strawberry Perl's 'c/bin' appearing before CMake in PATH.
This is known to cause CMake to misbehave (e.g., missing modules).
Please adjust your PATH so that:
C:\\Program Files\\CMake\\bin
appears before:
C:\\Strawberry\\c\\bin
You can do this in Environment Variables settings.
#############################################################
        ")

        if(NOT IS_CI)
            message(FATAL_ERROR "${_warning_text}")
        endif()
    endif()
endif ()

if (APPLE)
    if (NOT CMAKE_OSX_DEPLOYMENT_TARGET)
        set(CMAKE_OSX_DEPLOYMENT_TARGET "12.0" CACHE STRING "Minimum OS X deployment version" FORCE)
    endif ()
    message(STATUS "CMAKE_OSX_DEPLOYMENT_TARGET: ${CMAKE_OSX_DEPLOYMENT_TARGET}")
endif ()

if (POLICY CMP0092)
    cmake_policy(SET CMP0092 NEW)
endif ()

set(CMAKE_USER_MAKE_RULES_OVERRIDE "${CMAKE_CURRENT_LIST_DIR}/cmake/modules/ClangClShowIncludes.cmake")

project(OrcaSlicer)

if (CMAKE_SYSTEM_NAME STREQUAL "Linux" AND CMAKE_VERSION VERSION_LESS "3.25")
    set(LINUX ON CACHE BOOL "" FORCE)
endif ()

include("version.inc")
include(GNUInstallDirs)

if (NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  message(STATUS "No build type selected, default to Release")
  set(CMAKE_BUILD_TYPE "Release" CACHE STRING "Build type (default Release)" FORCE)
endif()

if (DEFINED BBL_RELEASE_TO_PUBLIC)
    add_compile_definitions("BBL_RELEASE_TO_PUBLIC=${BBL_RELEASE_TO_PUBLIC}")
    if (BBL_RELEASE_TO_PUBLIC)
        add_compile_definitions(WXINSPECTOR_DISABLE)
    endif ()
else ()
    add_compile_definitions("BBL_RELEASE_TO_PUBLIC=$<CONFIG:Release>")
    add_compile_definitions("$<$<CONFIG:Release>:WXINSPECTOR_DISABLE>")
endif ()

if(DEFINED ENV{SLIC3R_STATIC})
    set(SLIC3R_STATIC_INITIAL $ENV{SLIC3R_STATIC})
else()
    set(SLIC3R_STATIC_INITIAL 1)
endif()

option(SLIC3R_STATIC            "Compile OrcaSlicer with static libraries (Boost, TBB)" ${SLIC3R_STATIC_INITIAL})
option(SLIC3R_GUI               "Build the retired OrcaSlicer GUI" OFF)
if (SLIC3R_GUI)
    message(FATAL_ERROR "The engine is headless. Configure with SLIC3R_GUI=OFF.")
endif ()
option(SLIC3R_CAD               "Compile OrcaSlicer with the parametric Design/CAD tab (needs OCCT ModelingAlgorithms)" 1)
option(SLIC3R_PROFILE           "Compile OrcaSlicer with an invasive Shiny profiler" 0)
option(SLIC3R_PCH               "Use precompiled headers" 1)
option(SLIC3R_WARNINGS          "Emit compiler warnings for OrcaSlicer sources" 1)
option(SLIC3R_BUNDLED_WARNINGS  "Emit compiler warnings for bundled third-party sources" 0)
option(SLIC3R_MSVC_COMPILE_PARALLEL "Compile on Visual Studio in parallel" 1)
option(SLIC3R_MSVC_PDB          "Generate PDB files on MSVC in Release mode" 1)
option(SLIC3R_RELATIVE_DEBUG_PATHS "Record a relative compilation directory in debug info (clang-cl)" 0)
option(SLIC3R_ASAN              "Enable ASan on Clang and GCC" 0)

set(OPENVDB_FIND_MODULE_PATH "" CACHE PATH "Path to OpenVDB installation's find modules.")

set(IS_CROSS_COMPILE FALSE)

option (COLORED_OUTPUT "Always produce ANSI-colored output (GNU/Clang only)." OFF)
if (${COLORED_OUTPUT})
    if ("${CMAKE_CXX_COMPILER_ID}" STREQUAL "GNU")
        add_compile_options (-fdiagnostics-color=always)
    elseif ("${CMAKE_CXX_COMPILER_ID}" STREQUAL "Clang")
        add_compile_options (-fcolor-diagnostics)
    endif ()
endif ()

if (APPLE)
    list(LENGTH CMAKE_OSX_ARCHITECTURES _arch_len)
    if (_arch_len GREATER 1)
        message(FATAL_ERROR "OrcaSlicer only supports building for one architecture at a time. Please make sure only one architecture is specified in CMAKE_OSX_ARCHITECTURES")
    endif ()
    set(CMAKE_FIND_FRAMEWORK LAST)
    set(CMAKE_FIND_APPBUNDLE LAST)
    list(FIND CMAKE_OSX_ARCHITECTURES ${CMAKE_SYSTEM_PROCESSOR} _arch_idx)
    if (CMAKE_OSX_ARCHITECTURES AND _arch_idx LESS 0)
        set(IS_CROSS_COMPILE TRUE)
    endif ()
    if (CMAKE_MACOSX_BUNDLE)
        set(CMAKE_INSTALL_RPATH @executable_path/../Frameworks)
    endif()
    SET(CMAKE_XCODE_ATTRIBUTE_PRODUCT_BUNDLE_IDENTIFIER "com.orcaslicer.OrcaSlicer")

    set(CMAKE_XCODE_ATTRIBUTE_GCC_WARN_64_TO_32_BIT_CONVERSION "NO")

    message(STATUS "Orca: IS_CROSS_COMPILE: ${IS_CROSS_COMPILE}")
elseif (CMAKE_SYSTEM_NAME STREQUAL "Linux")
    set(CMAKE_INSTALL_RPATH "$ORIGIN")
endif ()

option(SLIC3R_BUILD_SANDBOXES   "Build development sandboxes" OFF)
option(BUILD_TESTS       "Build unit tests" OFF)
option(SLIC3R_BUILD_SLICE_TOOL "Build nonplanar_slice_tool without requiring unit tests" OFF)
option(ORCA_TOOLS       "Build Orca tools" OFF)

if (IS_CROSS_COMPILE)
    message("Detected cross compilation setup. Tests and encoding checks will be forcedly disabled!")
    set(BUILD_TESTS OFF CACHE BOOL "" FORCE)
endif ()

get_cmake_property(_cache_vars CACHE_VARIABLES)
list (SORT _cache_vars)
foreach (_cache_var ${_cache_vars})
    if("${_cache_var}" MATCHES "^SLIC3R_")
        message(STATUS "${_cache_var}: ${${_cache_var}}")
    endif ()
endforeach()

if (SLIC3R_CAD)
    add_definitions(-DSLIC3R_CAD)
endif ()

if(CMAKE_INSTALL_PREFIX_INITIALIZED_TO_DEFAULT)
    message(STATUS "Automatically setting CMAKE_INSTALL_PREFIX")
    set_property(CACHE CMAKE_INSTALL_PREFIX PROPERTY VALUE "${CMAKE_BINARY_DIR}/OrcaSlicer")
endif()

message(STATUS "CMAKE_INSTALL_PREFIX: ${CMAKE_INSTALL_PREFIX}")

if (MSVC AND CMAKE_CXX_COMPILER_ID STREQUAL Clang)
    set(IS_CLANG_CL TRUE)

    set(CMAKE_INCLUDE_SYSTEM_FLAG_CXX "-imsvc")
else ()
    set(IS_CLANG_CL FALSE)
endif ()

if (MSVC)
    string(REGEX REPLACE "/W[0-4]" "" CMAKE_C_FLAGS   "${CMAKE_C_FLAGS}")
    string(REGEX REPLACE "/W[0-4]" "" CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS}")

    if (SLIC3R_MSVC_COMPILE_PARALLEL AND CMAKE_GENERATOR MATCHES "Visual Studio")
           add_compile_options(/MP)
    endif ()

    if (NOT IS_CLANG_CL)
        if (MSVC_VERSION LESS 1928)
            message(FATAL_ERROR "Visual Studio 2019 16.8 (MSVC 19.28) or newer is required; detected MSVC ${MSVC_VERSION}.")
        endif ()
        add_compile_options(/Zc:lambda /Zc:preprocessor)
    endif ()
    add_compile_options(-bigobj)
    if (SLIC3R_MSVC_PDB)
        add_compile_options(/Zi /FS)
    endif ()
    add_compile_options(-D_SILENCE_CXX17_ADAPTOR_TYPEDEFS_DEPRECATION_WARNING)
    add_compile_options(/wd4244 /wd4267)
    add_compile_options(/wd4018)
    set(CMAKE_EXE_LINKER_FLAGS    "${CMAKE_EXE_LINKER_FLAGS} /LTCG")
    set(CMAKE_SHARED_LINKER_FLAGS "${CMAKE_SHARED_LINKER_FLAGS} /LTCG")
endif ()

if (SLIC3R_RELATIVE_DEBUG_PATHS)
    if (IS_CLANG_CL)
        add_compile_options(-ffile-compilation-dir=.)
    else ()
        message(WARNING "SLIC3R_RELATIVE_DEBUG_PATHS is only implemented for clang-cl")
    endif ()
endif ()

if (${CMAKE_CXX_COMPILER_ID} STREQUAL "AppleClang" AND ${CMAKE_CXX_COMPILER_VERSION} VERSION_GREATER 15)
    add_compile_definitions(BOOST_NO_CXX98_FUNCTION_BASE _HAS_AUTO_PTR_ETC=0)
endif()

if (MINGW)
    add_compile_options(-Wa,-mbig-obj)
endif ()

if (NOT MSVC)
    add_compile_options(-fsigned-char)
endif ()

if ("${DEP_BUILD_DIR}" STREQUAL "")
    get_filename_component(BIN_DIR_NAME ${CMAKE_BINARY_DIR} NAME)
    if (APPLE AND BIN_DIR_NAME STREQUAL "${CMAKE_OSX_ARCHITECTURES}")
        file(RELATIVE_PATH BIN_DIR_NAME ${CMAKE_BINARY_DIR}/../.. ${CMAKE_BINARY_DIR})
    endif ()
    set(DEP_BUILD_DIR "${CMAKE_SOURCE_DIR}/deps/${BIN_DIR_NAME}" CACHE PATH "Path to dependencies build directory" FORCE)
    message(STATUS "DEP_BUILD_DIR: ${DEP_BUILD_DIR} (generated automatically and saved to cache)")
    set(AUTOGENERATED_DEP_BUILD_DIR ${DEP_BUILD_DIR} CACHE PATH "Provides the last autogenerated DEP_BUILD_DIR" FORCE)
else ()
    message(STATUS "DEP_BUILD_DIR: ${DEP_BUILD_DIR} (from cache or command line)")
endif ()

if ("${CMAKE_PREFIX_PATH}" STREQUAL "" OR "${CMAKE_PREFIX_PATH}" STREQUAL "${AUTOGENERATED_PREFIX_PATH}")
    if (DEFINED AUTOGENERATED_DEP_BUILD_DIR AND NOT "${DEP_BUILD_DIR}" STREQUAL "${AUTOGENERATED_DEP_BUILD_DIR}")
        message(STATUS "CMAKE_PREFIX_PATH is being re-generated due to DEP_BUILD_DIR being manually updated")
        set(REGEN_DESTDIR TRUE)
        unset(AUTOGENERATED_DEP_BUILD_DIR CACHE)
    endif ()
else ()
    unset(AUTOGENERATED_PREFIX_PATH CACHE)
endif ()

if ("${CMAKE_PREFIX_PATH}" STREQUAL "" OR REGEN_DESTDIR)
    set(CMAKE_PREFIX_PATH "${DEP_BUILD_DIR}/OrcaSlicer_dep/usr/local" CACHE PATH "Path to dependencies install directory" FORCE)
    message(STATUS "CMAKE_PREFIX_PATH: ${CMAKE_PREFIX_PATH} (generated automatically and saved to cache)")
    set(AUTOGENERATED_PREFIX_PATH ${CMAKE_PREFIX_PATH} CACHE STRING "Provides the last autogenerated CMAKE_PREFIX_PATH" FORCE)
    unset(REGEN_DESTDIR CACHE)
else ()
    message(STATUS "CMAKE_PREFIX_PATH: ${CMAKE_PREFIX_PATH} (from cache or command line)")
endif ()
set(PREFIX_PATH_CHECK ${CMAKE_PREFIX_PATH})

if (NOT "$ENV{CMAKE_PREFIX_PATH}" STREQUAL "")
    message(STATUS "CMAKE_PREFIX_PATH: $ENV{CMAKE_PREFIX_PATH} (from environment)")
    list(APPEND PREFIX_PATH_CHECK $ENV{CMAKE_PREFIX_PATH})
endif ()

foreach (DIR ${PREFIX_PATH_CHECK})
    if (NOT EXISTS "${DIR}")
        message(WARNING "CMAKE_PREFIX_PATH element doesn't exist: ${DIR}")
    endif ()
endforeach ()

if (APPLE)
    set(CMAKE_MACOSX_RPATH ON CACHE BOOL "")
    set(CMAKE_MACOSX_BUNDLE ON CACHE BOOL "")
endif ()

if (APPLE AND CMAKE_MACOSX_RPATH AND "${CMAKE_INSTALL_RPATH}" STREQUAL "")
    set(CMAKE_INSTALL_RPATH ${CMAKE_PREFIX_PATH})
endif ()

list(APPEND CMAKE_MODULE_PATH ${PROJECT_SOURCE_DIR}/cmake/modules/)
message(STATUS "PROJECT_SOURCE_DIR: ${PROJECT_SOURCE_DIR}")
message(STATUS "CMAKE_MODULE_PATH: ${CMAKE_MODULE_PATH}")

enable_testing ()

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

if(NOT WIN32)
    add_compile_definitions("$<IF:$<CONFIG:Debug>,DEBUG,NDEBUG>")
endif()

set(CMAKE_POSITION_INDEPENDENT_CODE ON)

if (APPLE)
    message("OS X SDK Path: ${CMAKE_OSX_SYSROOT}")
    if (CMAKE_OSX_DEPLOYMENT_TARGET)
        message("OS X Deployment Target: ${CMAKE_OSX_DEPLOYMENT_TARGET}")
    else ()
        message("OS X Deployment Target: (default)")
    endif ()
endif ()

if (CMAKE_SYSTEM_NAME STREQUAL "Linux")
    find_package(PkgConfig REQUIRED)

    set(THREADS_PREFER_PTHREAD_FLAG ON)
    find_package(Threads REQUIRED)
endif()

if (CMAKE_COMPILER_IS_GNUCC OR CMAKE_COMPILER_IS_GNUXX)
    set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -fext-numeric-literals" )
endif()

if ((NOT MSVC OR IS_CLANG_CL) AND ("${CMAKE_CXX_COMPILER_ID}" STREQUAL "GNU" OR "${CMAKE_CXX_COMPILER_ID}" MATCHES "Clang"))
    if (IS_CLANG_CL)
        set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} /W4" )
        add_compile_options(-Wno-unused-parameter -Wno-ignored-qualifiers -Wno-missing-field-initializers)
    elseif (NOT MINGW)
        set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wall" )
    endif ()

    set(warnings_disabled
        reorder
        sign-compare
        misleading-indentation
        switch
        unused-function
        unused-variable
        unused-but-set-variable
        unused-label
        unused-local-typedefs
    )
    if (CMAKE_CXX_COMPILER_ID MATCHES "Clang")
        list(APPEND warnings_disabled deprecated-declarations)
    endif ()
    if (CMAKE_CXX_COMPILER_ID MATCHES "Clang" OR CMAKE_CXX_COMPILER_VERSION VERSION_GREATER 6.0)
        list(APPEND warnings_disabled ignored-attributes)
    endif ()
    if (CMAKE_CXX_COMPILER_ID STREQUAL "GNU")
        list(APPEND warnings_disabled unknown-pragmas)
    endif ()
    foreach (w IN LISTS warnings_disabled)
        add_compile_options(-Wno-${w})
    endforeach ()

    if (CMAKE_CXX_COMPILER_ID STREQUAL "GNU")
        add_compile_options(-Werror=return-type)
    else ()
        add_compile_options(-Werror)
    endif ()

    set(warnings_demoted)
    if (APPLE)
        list(APPEND warnings_demoted
            objc-method-access
        )
    endif ()
    if (WIN32 AND CMAKE_SYSTEM_PROCESSOR STREQUAL "ARM64")
        list(APPEND warnings_demoted
            cast-function-type-mismatch
        )
    endif ()
    if (CMAKE_CXX_COMPILER_ID MATCHES "Clang")
        list(APPEND warnings_demoted
            enum-constexpr-conversion
        )
    endif ()

    include(CheckCXXCompilerFlag)
    foreach (category IN LISTS warnings_demoted)
        string(MAKE_C_IDENTIFIER "ORCA_HAS_W_${category}" _orca_has_w)
        check_cxx_compiler_flag("-W${category}" ${_orca_has_w})
        if (${_orca_has_w})
            add_compile_options(-Wno-error=${category})
        else ()
            check_cxx_compiler_flag("-W${category}=1" ${_orca_has_w}_arg)
            if (${${_orca_has_w}_arg})
                add_compile_options(-Wno-error=${category}=)
            endif ()
        endif ()
    endforeach ()

    if(FLATPAK)
        if(("${CMAKE_CXX_COMPILER_ID}" STREQUAL "GNU" AND CMAKE_CXX_COMPILER_VERSION VERSION_GREATER 13.0) OR
           ("${CMAKE_CXX_COMPILER_ID}" MATCHES "Clang" AND CMAKE_CXX_COMPILER_VERSION VERSION_GREATER_EQUAL 15.0))
            add_compile_options(-gz=zstd)
        endif()
    endif()

endif()

if (SLIC3R_ASAN)
    add_compile_options(-fsanitize=address)

    if (NOT MSVC)
        add_compile_options(-fno-omit-frame-pointer)
        set(CMAKE_EXE_LINKER_FLAGS "${CMAKE_EXE_LINKER_FLAGS} -fsanitize=address")
        set(CMAKE_SHARED_LINKER_FLAGS "${CMAKE_SHARED_LINKER_FLAGS} -fsanitize=address")
        set(CMAKE_MODULE_LINKER_FLAGS "${CMAKE_MODULE_LINKER_FLAGS} -fsanitize=address")
    else()
        add_compile_definitions(_DISABLE_STRING_ANNOTATION=1 _DISABLE_VECTOR_ANNOTATION=1)
    endif ()

    if ("${CMAKE_CXX_COMPILER_ID}" STREQUAL "GNU")
        set(CMAKE_EXE_LINKER_FLAGS "${CMAKE_EXE_LINKER_FLAGS} -lasan")
    endif ()
endif ()

if (APPLE)
    set(CMAKE_C_FLAGS "${CMAKE_C_FLAGS} -Werror=partial-availability -Werror=unguarded-availability -Werror=unguarded-availability-new")
    set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Werror=partial-availability -Werror=unguarded-availability -Werror=unguarded-availability-new")
endif ()

if(MSVC)
add_compile_options(/wd4305)
endif()

set(LIBDIR ${CMAKE_CURRENT_SOURCE_DIR}/src)
message(STATUS "LIBDIR: ${LIBDIR}")

include_directories(SYSTEM ${LIBDIR})
include_directories(SYSTEM ${LIBDIR}/libigl)

if(WIN32)
    add_definitions(-D_USE_MATH_DEFINES -D_WIN32 -D_CRT_SECURE_NO_WARNINGS -D_SCL_SECURE_NO_WARNINGS)
    if(MSVC)
        add_definitions(-DBOOST_ALL_NO_LIB -DBOOST_USE_WINAPI_VERSION=0x602 -DBOOST_SYSTEM_USE_UTF8 )
        add_compile_options("$<$<C_COMPILER_ID:MSVC>:/utf-8>")
        add_compile_options("$<$<CXX_COMPILER_ID:MSVC>:/utf-8>")
    endif(MSVC)
endif(WIN32)

add_definitions(-DwxUSE_UNICODE -D_UNICODE -DUNICODE -DWXINTL_NO_GETTEXT_MACRO)

add_definitions(-DwxNO_UNSAFE_WXSTRING_CONV)

if (SLIC3R_PROFILE)
    message("OrcaSlicer will be built with a Shiny invasive profiler")
    add_definitions(-DSLIC3R_PROFILE)
endif ()

if(CMAKE_C_FLAGS_RELWITHDEBINFO MATCHES "/O2")
    string(REGEX REPLACE "/O2" "/Od" CMAKE_C_FLAGS_RELWITHDEBINFO "${CMAKE_C_FLAGS_RELWITHDEBINFO}")
endif()

if(CMAKE_CXX_FLAGS_RELWITHDEBINFO MATCHES "/O2")
    string(REGEX REPLACE "/O2" "/Od" CMAKE_CXX_FLAGS_RELWITHDEBINFO "${CMAKE_CXX_FLAGS_RELWITHDEBINFO}")
endif()

if("${CMAKE_CXX_COMPILER_ID}" MATCHES "Clang")
    if(CMAKE_C_FLAGS_RELWITHDEBINFO MATCHES "-O2")
        string(REGEX REPLACE "-O2" "-O0" CMAKE_C_FLAGS_RELWITHDEBINFO "${CMAKE_C_FLAGS_RELWITHDEBINFO}")
    endif()

    if(CMAKE_CXX_FLAGS_RELWITHDEBINFO MATCHES "-O2")
        string(REGEX REPLACE "-O2" "-O0" CMAKE_CXX_FLAGS_RELWITHDEBINFO "${CMAKE_CXX_FLAGS_RELWITHDEBINFO}")
    endif()
endif()

if(MSVC)
    if(CMAKE_C_FLAGS_RELWITHDEBINFO MATCHES "/Ob1")
        string(REGEX REPLACE "/Ob1" "/Ob0" CMAKE_C_FLAGS_RELWITHDEBINFO "${CMAKE_C_FLAGS_RELWITHDEBINFO}")
    endif()
    if(CMAKE_CXX_FLAGS_RELWITHDEBINFO MATCHES "/Ob1")
        string(REGEX REPLACE "/Ob1" "/Ob0" CMAKE_CXX_FLAGS_RELWITHDEBINFO "${CMAKE_CXX_FLAGS_RELWITHDEBINFO}")
    endif()

    set(CMAKE_C_FLAGS_RELWITHDEBINFO "${CMAKE_C_FLAGS_RELWITHDEBINFO} /RTC1")
    set(CMAKE_CXX_FLAGS_RELWITHDEBINFO "${CMAKE_CXX_FLAGS_RELWITHDEBINFO} /RTC1")
endif()

list(REMOVE_DUPLICATES CMAKE_C_FLAGS_RELWITHDEBINFO)
list(REMOVE_DUPLICATES CMAKE_CXX_FLAGS_RELWITHDEBINFO)

if(SLIC3R_STATIC)
    set(Boost_USE_STATIC_LIBS ON)
else()
    add_definitions(-DBOOST_LOG_DYN_LINK)
endif()
if(POLICY CMP0167)
  cmake_policy(SET CMP0167 NEW)
endif()
set(Boost_NO_SYSTEM_PATHS TRUE)
find_package(Boost 1.83.0 REQUIRED COMPONENTS system filesystem thread log log_setup locale regex chrono atomic date_time iostreams program_options nowide)

find_package(Eigen3 5.0.1 REQUIRED)

add_library(boost_libs INTERFACE)
add_library(boost_headeronly INTERFACE)

if (APPLE)
    target_compile_definitions(boost_headeronly INTERFACE BOOST_ASIO_DISABLE_KQUEUE)
endif()

if(NOT SLIC3R_STATIC)
    target_compile_definitions(boost_headeronly INTERFACE BOOST_LOG_DYN_LINK)
endif()

function(slic3r_remap_configs targets from_Cfg to_Cfg)
    if(MSVC)
        string(TOUPPER ${from_Cfg} from_CFG)

        foreach(tgt ${targets})
            if(TARGET ${tgt})
                set_target_properties(${tgt} PROPERTIES MAP_IMPORTED_CONFIG_${from_CFG} ${to_Cfg})
            endif()
        endforeach()
    endif()
endfunction()

target_include_directories(boost_headeronly SYSTEM INTERFACE ${Boost_INCLUDE_DIRS})
target_link_libraries(boost_libs INTERFACE boost_headeronly ${Boost_LIBRARIES})

if(SLIC3R_STATIC)
    set(TBB_STATIC 1)
endif()
set(TBB_DEBUG 1)
if ("${CMAKE_BUILD_TYPE}" STREQUAL "RelWithDebInfo" OR MSVC)
    set(CMAKE_MAP_IMPORTED_CONFIG_RELWITHDEBINFO RelWithDebInfo Release "")
endif()
find_package(TBB REQUIRED)

find_package(OpenSSL REQUIRED)
find_package(CURL REQUIRED)
find_package(Freetype REQUIRED)

add_library(libcurl INTERFACE)
target_link_libraries(libcurl INTERFACE CURL::libcurl)

find_package(ZLIB REQUIRED)
target_link_libraries(libcurl INTERFACE ZLIB::ZLIB)

if (NOT WIN32)
else()
    target_link_libraries(libcurl INTERFACE crypt32)
endif()

if (SLIC3R_STATIC AND NOT SLIC3R_STATIC_EXCLUDE_CURL)
    if (NOT APPLE)
        target_compile_definitions(libcurl INTERFACE CURL_STATICLIB)
    endif()
    if (CMAKE_SYSTEM_NAME STREQUAL "Linux")
        find_package(OpenSSL REQUIRED)
        message("OpenSSL include dir: ${OPENSSL_INCLUDE_DIR}")
        message("OpenSSL libraries: ${OPENSSL_LIBRARIES}")
        target_include_directories(libcurl SYSTEM INTERFACE ${OPENSSL_INCLUDE_DIR})
        target_link_libraries(libcurl INTERFACE ${OPENSSL_LIBRARIES})
    endif()
endif()

find_package(EXPAT)

if (NOT EXPAT_FOUND)
    add_library(expat STATIC
        ${LIBDIR}/expat/xmlparse.c
        ${LIBDIR}/expat/xmlrole.c
        ${LIBDIR}/expat/xmltok.c
    )
    set(EXPAT_FOUND 1)
    set(EXPAT_INCLUDE_DIRS ${LIBDIR}/expat/)
    set(EXPAT_LIBRARIES expat)
endif ()

find_package(PNG REQUIRED)

find_package(cereal REQUIRED)
if (NOT TARGET cereal::cereal)
    set_target_properties(cereal PROPERTIES IMPORTED_GLOBAL TRUE)
    add_library(cereal::cereal ALIAS cereal)
else ()
    set_target_properties(cereal::cereal PROPERTIES IMPORTED_GLOBAL TRUE)
endif ()

find_package(NLopt 1.4 REQUIRED)

set(_bundled_python_version "3.12.13")
set(_bundled_python_abi "312")
string(REGEX REPLACE "^([0-9]+\\.[0-9]+)\\..*$" "\\1" _bundled_python_version_short "${_bundled_python_version}")
set(_bundled_python_root "${CMAKE_PREFIX_PATH}/libpython")
set(Python3_ROOT_DIR "${_bundled_python_root}" CACHE PATH "Root directory for bundled Python" FORCE)
set(Python3_USE_STATIC_LIBS OFF)
set(Python3_FIND_STRATEGY LOCATION)
set(Python3_FIND_IMPLEMENTATIONS CPython)

if(WIN32)
    set(_bundled_python_executable "${_bundled_python_root}/python.exe")
    set(_bundled_python_library "${_bundled_python_root}/libs/python${_bundled_python_abi}.lib")
    if(EXISTS "${_bundled_python_root}/python_d.exe")
        set(_bundled_python_executable "${_bundled_python_root}/python_d.exe")
    endif()
    if(EXISTS "${_bundled_python_root}/libs/python${_bundled_python_abi}_d.lib")
        set(_bundled_python_library "${_bundled_python_root}/libs/python${_bundled_python_abi}_d.lib")
    endif()

    set(Python3_FIND_REGISTRY NEVER)
    set(Python3_EXECUTABLE "${_bundled_python_executable}" CACHE FILEPATH "Bundled Python executable" FORCE)
    set(Python3_INCLUDE_DIR "${_bundled_python_root}/include" CACHE PATH "Bundled Python include directory" FORCE)
    set(Python3_LIBRARY "${_bundled_python_library}" CACHE FILEPATH "Bundled Python embed import library" FORCE)
elseif(APPLE)
    set(Python3_FIND_FRAMEWORK NEVER)

    find_program(_bundled_python_executable
        NAMES python${_bundled_python_version_short} python3
        PATHS "${_bundled_python_root}/bin"
        NO_DEFAULT_PATH
    )
    find_path(_bundled_python_include_dir
        NAMES Python.h
        PATHS
            "${_bundled_python_root}/include/python${_bundled_python_version_short}"
            "${_bundled_python_root}/include"
        NO_DEFAULT_PATH
    )
    find_library(_bundled_python_library
        NAMES python${_bundled_python_version_short} libpython${_bundled_python_version_short}
        PATHS "${_bundled_python_root}/lib"
        NO_DEFAULT_PATH
    )

    if(NOT _bundled_python_executable)
        message(FATAL_ERROR "Bundled Python executable not found under ${_bundled_python_root}/bin")
    endif()
    if(NOT _bundled_python_include_dir)
        message(FATAL_ERROR "Bundled Python headers not found under ${_bundled_python_root}/include")
    endif()
    if(NOT _bundled_python_library)
        message(FATAL_ERROR "Bundled Python library not found under ${_bundled_python_root}/lib")
    endif()

    set(Python3_EXECUTABLE "${_bundled_python_executable}" CACHE FILEPATH "Bundled Python executable" FORCE)
    set(Python3_INCLUDE_DIR "${_bundled_python_include_dir}" CACHE PATH "Bundled Python include directory" FORCE)
    set(Python3_LIBRARY "${_bundled_python_library}" CACHE FILEPATH "Bundled Python embed library" FORCE)
endif()

find_package(Python3 ${_bundled_python_version} EXACT REQUIRED
    COMPONENTS Interpreter Development.Embed
)

set(PYBIND11_SOURCE_DIR "${CMAKE_SOURCE_DIR}/deps_src/pybind11")
if(NOT EXISTS "${PYBIND11_SOURCE_DIR}/include/pybind11/pybind11.h")
    message(FATAL_ERROR "pybind11 headers not found in ${PYBIND11_SOURCE_DIR}. Did you initialize submodules?")
endif()

add_library(pybind11_headers INTERFACE)
target_include_directories(pybind11_headers INTERFACE "${PYBIND11_SOURCE_DIR}/include")
add_library(pybind11::headers ALIAS pybind11_headers)
add_library(pybind11::pybind11 ALIAS pybind11_headers)

add_library(pybind11_embed INTERFACE)
target_link_libraries(pybind11_embed INTERFACE pybind11_headers Python3::Python)
target_compile_definitions(pybind11_embed INTERFACE PYBIND11_SIMPLE_GIL_MANAGEMENT)
add_library(pybind11::embed ALIAS pybind11_embed)


if(SLIC3R_STATIC)
    set(OPENVDB_USE_STATIC_LIBS ON)
    set(USE_BLOSC TRUE)
endif ()

find_package(OpenVDB 5.0 COMPONENTS openvdb)
if(OpenVDB_FOUND)
    slic3r_remap_configs(IlmBase::Half RelWithDebInfo Release)
    slic3r_remap_configs(Blosc::blosc RelWithDebInfo Release)
else ()
    message(FATAL_ERROR "OpenVDB could not be found with the bundled find module. "
                   "You can try to specify the find module location of your "
                   "OpenVDB installation with the OPENVDB_FIND_MODULE_PATH cache variable.")
endif ()

set(TOP_LEVEL_PROJECT_DIR ${PROJECT_SOURCE_DIR})
function(orcaslicer_copy_dlls config)
    if ("${CMAKE_SYSTEM_PROCESSOR}" STREQUAL "AMD64")
        set(_arch "x64")
    elseif ("${CMAKE_SYSTEM_PROCESSOR}" STREQUAL "X86")
        set(_arch "x86")
    elseif ("${CMAKE_SYSTEM_PROCESSOR}" STREQUAL "ARM64")
        set(_arch "arm64")
    else ()
        message(FATAL_ERROR "Unable to detect architecture: ${CMAKE_SYSTEM_PROCESSOR}")
    endif ()

    get_property(_is_multi GLOBAL PROPERTY GENERATOR_IS_MULTI_CONFIG)
    if (_is_multi)
        set(_out_dir "${CMAKE_CURRENT_BINARY_DIR}/${config}")
    else ()
        set(_out_dir "${CMAKE_CURRENT_BINARY_DIR}")
    endif ()

    file(COPY ${TOP_LEVEL_PROJECT_DIR}/deps/GMP/gmp/lib/win-${_arch}/libgmp-10.dll
            ${TOP_LEVEL_PROJECT_DIR}/deps/MPFR/mpfr/lib/win-${_arch}/libmpfr-4.dll
        DESTINATION ${_out_dir})

    if (NOT OCCT_LIBS)
        message(FATAL_ERROR "OCCT_LIBS is not set; libslic3r must be configured first.")
    endif ()
    string(TOUPPER "${config}" _config_upper)
    set(_occt_bin "${OCCT_BIN_DIR_${_config_upper}}")
    set(_occt_dlls "")
    set(_missing_occt "")
    foreach (_tk IN LISTS OCCT_LIBS)
        if (EXISTS "${_occt_bin}/${_tk}.dll")
            list(APPEND _occt_dlls "${_occt_bin}/${_tk}.dll")
        else ()
            list(APPEND _missing_occt "${_tk}.dll")
        endif ()
    endforeach ()
    if (_missing_occt)
        message(FATAL_ERROR
            "OCCT DLLs missing from ${_occt_bin}/: ${_missing_occt}\n"
            "Rebuild the dependencies (build_release_vs2022.bat deps) with the same "
            "SLIC3R_CAD setting as this project.")
    endif ()
    file(COPY ${_occt_dlls}
            ${CMAKE_PREFIX_PATH}/bin/freetype.dll
         DESTINATION ${_out_dir})
endfunction()

function(orcaslicer_silence_third_party_warnings _dir)
    get_property(_subdirs DIRECTORY "${_dir}" PROPERTY SUBDIRECTORIES)
    foreach (_subdir IN LISTS _subdirs)
        orcaslicer_silence_third_party_warnings("${_subdir}")
    endforeach ()
    get_property(_targets DIRECTORY "${_dir}" PROPERTY BUILDSYSTEM_TARGETS)
    foreach (_target IN LISTS _targets)
        get_target_property(_type ${_target} TYPE)
        if (NOT _type STREQUAL "INTERFACE_LIBRARY" AND NOT _type STREQUAL "UTILITY")
            if (MSVC AND NOT IS_CLANG_CL)
                get_target_property(_opts ${_target} COMPILE_OPTIONS)
                if (_opts)
                    string(REGEX REPLACE "/W[0-4]|/Wall" "" _opts "${_opts}")
                    string(REGEX REPLACE ";;+" ";" _opts "${_opts}")
                    set_target_properties(${_target} PROPERTIES COMPILE_OPTIONS "${_opts}")
                endif ()
                target_compile_options(${_target} PRIVATE /W0)
            else ()
                target_compile_options(${_target} PRIVATE -w)
            endif ()
        endif ()
    endforeach ()
endfunction()


add_subdirectory(deps_src)

if (NOT SLIC3R_BUNDLED_WARNINGS)
    orcaslicer_silence_third_party_warnings("${CMAKE_CURRENT_SOURCE_DIR}/deps_src")
endif ()

if (NOT SLIC3R_WARNINGS)
    add_compile_options(-w)
elseif (MSVC AND NOT IS_CLANG_CL)
    add_compile_options(/W3 /we4715)
endif ()

add_subdirectory(src)
if(SLIC3R_BUILD_SANDBOXES)
    add_subdirectory(sandboxes)
endif()

if(BUILD_TESTS OR SLIC3R_BUILD_SLICE_TOOL)
    add_subdirectory(tests)
    if (BUILD_TESTS AND NOT SLIC3R_BUNDLED_WARNINGS)
        orcaslicer_silence_third_party_warnings("${CMAKE_CURRENT_SOURCE_DIR}/tests/catch2")
    endif ()
endif()

install(DIRECTORY resources/ DESTINATION resources)
install(FILES LICENSE.txt DESTINATION ".")
