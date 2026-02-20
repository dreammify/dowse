package com.example.core

fun String.capitalizeWords(): String =
    split(" ").joinToString(" ") { word ->
        word.replaceFirstChar { it.uppercaseChar() }
    }

fun String.truncate(maxLength: Int, ellipsis: String = "..."): String =
    if (length <= maxLength) this
    else take(maxLength - ellipsis.length) + ellipsis
