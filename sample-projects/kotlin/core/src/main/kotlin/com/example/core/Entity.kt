package com.example.core

abstract class Entity {
    abstract fun validate(): Result<Unit>

    fun isValid(): Boolean = validate() is Result.Success
}
