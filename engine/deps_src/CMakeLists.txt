cmake_minimum_required(VERSION 3.13)
project(deps_src)

# Include all dependency subdirectories

# Header-only libraries (INTERFACE)
add_subdirectory(agg)
add_subdirectory(ankerl)
add_subdirectory(earcut)
add_subdirectory(fast_float)
add_subdirectory(nanosvg)
add_subdirectory(nlohmann)
add_subdirectory(stb_dxt)     # Header-only STB DXT compression library

# Static libraries
if (SLIC3R_PROFILE)
    add_subdirectory(Shiny)
else()
    add_subdirectory(Shiny EXCLUDE_FROM_ALL)
endif()
add_subdirectory(admesh)
add_subdirectory(clipper2)
add_subdirectory(expat)
add_subdirectory(glu-libtess)
add_subdirectory(libigl)
add_subdirectory(libnest2d)
add_subdirectory(mcut)
add_subdirectory(miniz)
add_subdirectory(qhull)
add_subdirectory(qoi)
add_subdirectory(semver)      # Semver static library
