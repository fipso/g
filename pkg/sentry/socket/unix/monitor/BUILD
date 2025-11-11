load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "monitor",
    srcs = [
        "monitor.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/log",
    ],
)
