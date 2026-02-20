package com.example.models

import com.example.core.Result

class UserRepository {
    private val users = mutableMapOf<Long, User>()

    fun save(user: User): Result<User> {
        val validation = user.validate()
        if (validation is Result.Failure) return Result.Failure(validation.error)
        users[user.id] = user
        return Result.Success(user)
    }

    fun findById(id: Long): Result<User> {
        val user = users[id] ?: return Result.Failure("User not found: $id")
        return Result.Success(user)
    }

    fun findAll(): List<User> = users.values.toList()
}
