file(REMOVE_RECURSE
  "libyuv.dll"
  "libyuv.dll.a"
  "libyuv.dll.manifest"
  "libyuv.pdb"
)

# Per-language clean rules from dependency scanning.
foreach(lang CXX)
  include(CMakeFiles/yuv_shared.dir/cmake_clean_${lang}.cmake OPTIONAL)
endforeach()
