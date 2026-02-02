load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "netmonitor",
    srcs = [
        "netmonitor.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/log",
    ],
)
