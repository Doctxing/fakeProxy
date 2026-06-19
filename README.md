# fakeProxy

[中文](README_CN.md)

fakeProxy is a small proxy tool written in Go. It rewrites page resources to local proxy addresses, keeping browser traffic inside the local proxy flow as much as possible. This makes it easier for other devices to access relatively simple authentication sites as if they were the headless device. In actual testing, it has also shown decent compatibility with some more complex sites.

The idea behind it is that for relatively simple websites, the tested delivery flow often does not strongly depend on the original source domain. In other words, internal paths are very likely to be relative paths. This kind of fake proxy access usually does not have a negative impact on those sites, and most sites do not intentionally defend against this workflow. The current replacement rules do not include `javascript`; if the current rewrite rules are not enough, a hook function is provided for custom handling.
