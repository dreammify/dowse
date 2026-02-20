package com.example.models

import com.example.core.Entity
import com.example.core.Identifiable
import com.example.core.Result

data class User(
    override val id: Long,
    val name: String,
    val email: String,
    val permission: Permission
) : Entity(), Identifiable<Long> {

    override fun validate(): Result<Unit> = when {
        name.isBlank() -> Result.Failure("Name must not be blank")
        !email.contains("@") -> Result.Failure("Invalid email format")
        else -> Result.Success(Unit)
    }
}
